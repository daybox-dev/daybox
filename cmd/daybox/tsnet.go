package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tailscale.com/tsnet"
)

type netFlags struct {
	control     string
	state       string
	hostname    string
	authkey     string // set programmatically (enroll), not by flag
	authkeyFile string
	verbose     bool
}

func addNetFlags(fs *flag.FlagSet, defState string) *netFlags {
	f := &netFlags{}
	host, _ := os.Hostname()
	fs.StringVar(&f.control, "control", loadConfig().controlURL(), "coordination server URL")
	fs.StringVar(&f.state, "state", defState, "tsnet state directory")
	fs.StringVar(&f.hostname, "hostname", strings.Split(host, ".")[0], "node hostname on the net")
	fs.StringVar(&f.authkeyFile, "authkey-file", "", "file containing a pre-auth key")
	fs.BoolVar(&f.verbose, "v", false, "verbose tsnet logging")
	return f
}

func defaultStateDir() string {
	// deliberately NOT os.UserConfigDir(): on macOS that's ~/Library/…,
	// and everything else daybox keeps machine-local lives in ~/.config/daybox
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".config", "daybox", "tsnet")
	}
	return ".daybox-tsnet"
}

func (f *netFlags) authKey() string {
	if f.authkey != "" {
		return f.authkey
	}
	if f.authkeyFile != "" {
		b, err := os.ReadFile(f.authkeyFile)
		if err != nil {
			log.Fatalf("authkey-file: %v", err)
		}
		return strings.TrimSpace(string(b))
	}
	return os.Getenv("TS_AUTHKEY")
}

func (f *netFlags) server(ephemeral bool) *tsnet.Server {
	if f.control == "" {
		log.Fatal("no coordination server: run 'daybox init', or set -control / $DAYBOX_CONTROL")
	}
	s := &tsnet.Server{
		Dir:        f.state,
		Hostname:   f.hostname,
		ControlURL: f.control,
		AuthKey:    f.authKey(),
		Ephemeral:  ephemeral,
	}
	if !f.verbose {
		s.Logf = func(string, ...any) {}
		s.UserLogf = func(string, ...any) {}
	}
	return s
}

func up(s *tsnet.Server) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	st, err := s.Up(ctx)
	if err != nil {
		log.Fatalf("up: %v", err)
	}
	for _, ip := range st.TailscaleIPs {
		if ip.Is4() {
			return ip.String()
		}
	}
	if len(st.TailscaleIPs) > 0 {
		return st.TailscaleIPs[0].String()
	}
	return ""
}

// join: the devbox side. Ephemeral node; anything that connects to this
// node's :port on the net is proxied to -target (the box's own sshd), so
// sshd itself never listens on the net — the agent is the only net surface.
func cmdJoin(p Parsed) {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	f := addNetFlags(fs, "/var/lib/daybox-agent")
	port := fs.Int("port", 22, "port to expose on the net")
	target := fs.String("target", "127.0.0.1:22", "local address to proxy to")
	relayLocal := fs.String("relay-local", fmt.Sprintf("127.0.0.1:%d", relayDefaultPort),
		"localhost listener proxied to the control plane's relay (empty disables)")
	relayPeer := fs.String("relay-peer", fmt.Sprintf("daybox-relay:%d", relayDefaultPort),
		"the relay's name:port on the net")
	fs.Parse(p.Rest())

	s := f.server(true)
	defer s.Close()
	ip := up(s)
	log.Printf("joined as %s (%s)", ip, f.hostname)

	// `daybox profile propose` needs to reach the relay AS this box: net
	// identity lives in this process, so the agent lends it — a localhost
	// door that leads to exactly one place, the relay's inert proposal
	// intake (it opens no new inbound surface)
	if *relayLocal != "" {
		go relayProxy(s, *relayLocal, *relayPeer)
	}

	ln, err := s.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			time.Sleep(time.Second)
			continue
		}
		go proxy(c, *target)
	}
}

// relayProxy accepts on a localhost address and pipes each connection to
// the relay over the net, resolving the relay's bare peer name per
// connection (the relay may enroll after this box did).
func relayProxy(s *tsnet.Server, local, peer string) {
	ln, err := net.Listen("tcp", local)
	if err != nil {
		log.Printf("relay proxy: %v (propose disabled on this box)", err)
		return
	}
	log.Printf("relay proxy: %s → %s", local, peer)
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("relay proxy accept: %v", err)
			time.Sleep(time.Second)
			continue
		}
		go func() {
			defer c.Close()
			host, port, err := net.SplitHostPort(peer)
			if err != nil {
				log.Printf("relay proxy: bad peer %q: %v", peer, err)
				return
			}
			if net.ParseIP(host) == nil {
				if ip, ok := resolvePeer(s, host); ok {
					host = ip
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			t, err := s.Dial(ctx, "tcp", net.JoinHostPort(host, port))
			cancel()
			if err != nil {
				log.Printf("relay proxy dial: %v (is daybox-relay enabled on the control plane?)", err)
				return
			}
			defer t.Close()
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); io.Copy(t, c); halfClose(t) }()
			go func() { defer wg.Done(); io.Copy(c, t); halfClose(c) }()
			wg.Wait()
		}()
	}
}

type closeWriter interface{ CloseWrite() error }

func halfClose(c net.Conn) {
	if cw, ok := c.(closeWriter); ok {
		cw.CloseWrite()
	} else {
		c.Close()
	}
}

func proxy(c net.Conn, target string) {
	defer c.Close()
	t, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		log.Printf("proxy dial %s: %v", target, err)
		return
	}
	defer t.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(t, c); halfClose(t) }()
	go func() { defer wg.Done(); io.Copy(c, t); halfClose(c) }()
	wg.Wait()
}

// dial: the device side. ssh ProxyCommand contract — connection bytes on
// stdout, everything else on stderr.
func cmdDial(p Parsed) {
	fs := flag.NewFlagSet("dial", flag.ExitOnError)
	f := addNetFlags(fs, defaultStateDir())
	fs.Parse(p.Rest())
	rest := fs.Args()
	if len(rest) != 2 {
		log.Fatal("usage: daybox dial [flags] HOST PORT")
	}
	host, port := rest[0], rest[1]

	s := f.server(false)
	defer s.Close()
	up(s)

	// bare names ("daybox") aren't resolvable by the in-process DNS without a
	// search domain — match them against the peer list instead, so plain
	// `ssh dev@daybox` works as a ProxyCommand target.
	if net.ParseIP(host) == nil && !strings.Contains(host, ".") {
		if ip, ok := resolvePeer(s, host); ok {
			host = ip
		}
	}
	addr := net.JoinHostPort(host, port)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	c, err := s.Dial(ctx, "tcp", addr)
	cancel()
	if err != nil {
		log.Fatalf("dial %s: %v", addr, err)
	}
	go func() {
		io.Copy(c, os.Stdin)
		halfClose(c)
	}()
	io.Copy(os.Stdout, c) // returns when the remote side closes
}

func resolvePeer(s *tsnet.Server, name string) (string, bool) {
	lc, err := s.LocalClient()
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := lc.Status(ctx)
	if err != nil {
		return "", false
	}
	for _, p := range st.Peer {
		if strings.EqualFold(strings.Split(p.DNSName, ".")[0], name) ||
			strings.EqualFold(p.HostName, name) {
			for _, ip := range p.TailscaleIPs {
				if ip.Is4() {
					return ip.String(), true
				}
			}
		}
	}
	return "", false
}

func cmdIP(p Parsed) {
	fs := flag.NewFlagSet("ip", flag.ExitOnError)
	f := addNetFlags(fs, defaultStateDir())
	fs.Parse(p.Rest())

	s := f.server(false)
	defer s.Close()
	fmt.Println(up(s))
}

// enroll: put THIS device on the net, narrating each step. Requires ssh
// access to the control plane (that access is the credential —
// the pairing dashboard later replaces this ceremony).
func cmdEnroll(p Parsed) {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	f := addNetFlags(fs, defaultStateDir())
	device := fs.String("device", "", "name for this device on the net (default: hostname)")
	fs.Parse(p.Rest())
	if *device != "" {
		f.hostname = *device
	}
	cfg := loadConfig()
	enroll(cfg, f)
}

func enroll(cfg *config, f *netFlags) {
	control := cfg.controlHost()
	if control == "" {
		log.Fatal("no control plane configured — run: daybox init")
	}
	if f.control == "" {
		f.control = cfg.controlURL()
	}
	netUser := cfg.get("NET_USER", "dev")

	say("enrolling this device on your net as %q —", f.hostname)
	say("  only enrolled devices can reach your boxes; membership expires and is re-earnable.")

	say("• minting a single-use enrollment key on %s (valid 15 minutes)", control)
	uid := headscaleUserID(control, netUser)
	out, err := sshCapture(control, fmt.Sprintf(
		"headscale preauthkeys create --user %s --expiration 15m", uid))
	if err != nil {
		log.Fatalf("could not mint enrollment key on %s: %v", control, err)
	}
	// last whitespace-separated token is the key; headscale prints human CLI
	// output, so guard the parse — empty output (or a key on stderr in some
	// future version) must fail with the output shown, not panic
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 {
		log.Fatalf("could not parse an enrollment key out of headscale's output: %q", out)
	}
	key := fields[len(fields)-1]

	say("• joining the net (userspace node — no VPN profile, no admin rights)")
	f.authkey = key
	s := f.server(false)
	defer s.Close()
	ip := up(s)

	// pin the display name so OS hostname drift never renames the node;
	// quoted — the hostname can come from the OS, not just our validated flag
	if id, name := headscaleNodeByIP(control, ip); id != "" && name != f.hostname {
		sshCapture(control, fmt.Sprintf("headscale nodes rename -i %s %s", id, shQuote(f.hostname)))
	}

	say("✓ this device is %q at %s on your net", f.hostname, ip)
	say("  identity lives in %s — re-run 'daybox enroll' when it expires.", f.state)
}

func headscaleUserID(control, name string) string {
	out, err := sshCapture(control, "headscale users list -o json")
	if err != nil {
		log.Fatalf("headscale not reachable on %s: %v", control, err)
	}
	var users []struct {
		ID   json.Number `json:"id"`
		Name string      `json:"name"`
	}
	json.Unmarshal([]byte(out), &users)
	for _, u := range users {
		if u.Name == name {
			return u.ID.String()
		}
	}
	log.Fatalf("no headscale user %q on %s (run: daybox init)", name, control)
	return ""
}

func headscaleNodeByIP(control, ip string) (id, givenName string) {
	out, err := sshCapture(control, "headscale nodes list -o json")
	if err != nil {
		return "", ""
	}
	var nodes []struct {
		ID        json.Number `json:"id"`
		GivenName string      `json:"given_name"`
		IPs       []string    `json:"ip_addresses"`
	}
	json.Unmarshal([]byte(out), &nodes)
	for _, n := range nodes {
		for _, a := range n.IPs {
			if a == ip {
				return n.ID.String(), n.GivenName
			}
		}
	}
	return "", ""
}
