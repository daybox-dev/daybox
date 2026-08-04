package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"
)

// daybox init — the one-size-fits-all bootstrap (north star:
// "init once, up forever"). Interviews for what it can't infer, provisions
// (or adopts) the control plane, and enrolls this device. Idempotent: every
// remote step skips what already exists, so re-running heals drift.
//
// Runs either from a repo checkout (--repo, or auto-detected from cwd — the
// developer path) or, with no checkout at all, from a pinned and signed
// release payload downloaded on the fly (the curl-installed path). Both
// produce a directory laid out the same way, so everything below this line is
// identical either way. See payload.go.

type initOpts struct {
	adopt      string // ssh destination of an existing box
	provision  bool   // create a fresh Hetzner VPS instead
	tokenFile  string
	location   string
	serverType string
	name       string // provisioned VPS name
	user       string // login user on a provisioned VPS
	gitName    string
	gitEmail   string
	device     string
	repo       string
	version    string
	noEnroll   bool
}

func cmdInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	o := &initOpts{}
	fs.StringVar(&o.adopt, "adopt", "", "adopt an existing box (ssh destination, e.g. me@1.2.3.4 or an ssh alias)")
	fs.BoolVar(&o.provision, "provision", false, "provision a new Hetzner VPS as the control plane")
	fs.StringVar(&o.tokenFile, "token-file", "", "file holding the Hetzner API token (provision)")
	fs.StringVar(&o.location, "location", "hil", "Hetzner location (provision)")
	fs.StringVar(&o.serverType, "server-type", "cpx11", "Hetzner server type for the control plane (provision)")
	fs.StringVar(&o.name, "name", "daybox-control", "server name (provision)")
	fs.StringVar(&o.user, "user", "daybox", "login user to create on a provisioned VPS")
	fs.StringVar(&o.gitName, "git-name", "", "git author name for work done on your boxes")
	fs.StringVar(&o.gitEmail, "git-email", "", "git author email")
	fs.StringVar(&o.device, "device", "", "this device's name on the net (default: hostname)")
	fs.StringVar(&o.repo, "repo", "", "daybox repo checkout (default: auto-detect from cwd)")
	fs.StringVar(&o.version, "version", "", "release payload to provision from (default: a checkout if present, else latest)")
	fs.BoolVar(&o.noEnroll, "no-enroll", false, "skip enrolling this device")
	fs.Parse(args)

	say("daybox init — one-time setup.")
	say("")
	say("A control plane is a small always-on box that summons your beefy")
	say("hourly devboxes, reaps them when idle, and coordinates the private")
	say("net your devices use to reach them. It bills to your own provider")
	say("account; you'll see the live price before anything is created.")
	say("")

	repo, cleanupPayload := resolvePayload(o.repo, o.version)
	defer cleanupPayload()

	// ---- interview (only for what flags didn't provide) ----
	in := bufio.NewReader(os.Stdin)
	if o.adopt == "" && !o.provision {
		choice := prompt(in, "Control plane — [1] provision a new Hetzner VPS  [2] adopt a box I can ssh into", "2")
		if strings.TrimSpace(choice) == "1" {
			o.provision = true
		} else {
			o.adopt = prompt(in, "ssh destination of that box (alias or user@host)", "")
			if o.adopt == "" {
				log.Fatal("need an ssh destination (or re-run with --provision)")
			}
		}
	}
	if o.gitName == "" {
		o.gitName = prompt(in, "git author name (used on your boxes)", gitDefault("user.name"))
	}
	if o.gitEmail == "" {
		o.gitEmail = prompt(in, "git author email", gitDefault("user.email"))
	}
	if o.gitName == "" || o.gitEmail == "" {
		log.Fatal("git identity is required (boxes commit as you)")
	}
	if o.device == "" {
		// The suggested default must itself pass validDeviceName — a stock
		// mac hostname is "Alices-MacBook-Air", and offering a default we
		// then refuse aborts the whole init on <enter>.
		host, _ := os.Hostname()
		def := sanitizeDeviceName(strings.Split(host, ".")[0])
		for tries := 0; ; tries++ {
			o.device = prompt(in, "name for this device on your net", def)
			if validDeviceName(o.device) {
				break
			}
			if tries == 4 {
				log.Fatalf("device name %q: use lowercase letters, digits and dashes", o.device)
			}
			say("  %q won't work: use lowercase letters, digits and dashes", o.device)
		}
	}
	// These values travel into remote shell commands, file paths and a
	// rendered cloud-init script. Quoting (shQuote below) handles metachars;
	// newlines and control characters have no safe representation end-to-end,
	// and the device name is also a path component and a hostname.
	if strings.ContainsAny(o.gitName+o.gitEmail, "\n\r\x00") {
		log.Fatal("git name/email must be single-line values")
	}
	if !validDeviceName(o.device) {
		log.Fatalf("device name %q: use lowercase letters, digits and dashes", o.device)
	}
	// o.user lands unquoted inside a root shell heredoc on a fresh VPS
	// (provisionControlPlane) and as an ssh destination — same "keep it
	// boring" rule as the device name, checked with the same strictness.
	if !validDeviceName(o.user) {
		log.Fatalf("user %q: use lowercase letters, digits and dashes", o.user)
	}

	// ---- control plane ----
	control := o.adopt
	var freshToken string
	if o.provision {
		control, freshToken = provisionControlPlane(in, o)
	}

	say("• checking ssh access to %s", control)
	if _, err := sshCapture(control, "true"); err != nil {
		log.Fatalf("cannot ssh to %s: %v", control, err)
	}
	publicIP, err := sshCapture(control, "curl -4fsS --max-time 10 https://api.ipify.org")
	if err != nil || strings.TrimSpace(publicIP) == "" {
		log.Fatalf("could not learn %s's public IP: %v", control, err)
	}
	publicIP = strings.TrimSpace(publicIP)
	say("  reachable; public IP %s", publicIP)

	// ---- push the tool ----
	if _, err := sshCapture(control, "test -d ~/daybox"); err != nil {
		say("• installing the daybox tree to ~/daybox on the control plane")
		if err := pushTree(repo, control); err != nil {
			log.Fatalf("pushing repo: %v", err)
		}
	} else {
		say("• ~/daybox already present on %s — leaving it as-is ('daybox upgrade' replaces it)", control)
	}

	// agent binary for devbox pushes + this device's key for box access
	say("• installing the net agent + this device's ssh key on the control plane")
	agentBin := filepath.Join(repo, "dist", "daybox-agent-linux-amd64")
	if _, err := os.Stat(agentBin); err != nil {
		log.Fatalf("missing %s\n  from a checkout: run cmd/daybox/build.sh first\n  from a release: the payload is incomplete — report it", agentBin)
	}
	sshCapture(control, "mkdir -p ~/.config/daybox/agent ~/.config/daybox/keys")
	if err := scpTo(agentBin, control, ".config/daybox/agent/daybox-agent"); err != nil {
		log.Fatal(err)
	}
	pub := findLocalPubkey()
	if err := scpTo(pub, control, ".config/daybox/keys/"+o.device+".pub"); err != nil {
		log.Fatal(err)
	}

	// token: a provisioned plane needs the same token to summon boxes
	if freshToken != "" {
		say("• storing the Hetzner token on the control plane (mode 600; it never leaves that box)")
		// the reader is recreated each attempt so a transport retry re-pipes
		// the full token — a reader consumed by attempt 1 would send EOF on
		// attempt 2 and silently store an empty token.
		err := sshRetry("control plane", func() error {
			c := exec.Command("ssh", append(sshOpts(true), control,
				"install -m 600 /dev/stdin ~/.config/daybox/token")...)
			c.Stdin = strings.NewReader(freshToken)
			c.Stderr = os.Stderr
			return c.Run()
		})
		if err != nil {
			log.Fatal(err)
		}
	}

	// ---- run the idempotent setup script ----
	// Lands in $HOME, not /tmp: a predictable path in a world-writable dir
	// is a symlink/pre-creation race on any multi-user box we might adopt.
	// Values are shell-quoted — Go's %q does not escape $ or backticks, and
	// this string is parsed by the remote POSIX shell.
	say("• configuring the control plane (packages, coordination server, reaper)")
	if err := scpTo(filepath.Join(repo, "remote", "controlplane-setup.sh"),
		control, ".daybox-controlplane-setup.sh"); err != nil {
		log.Fatal(err)
	}
	setup := fmt.Sprintf("PUBLIC_IP=%s GIT_NAME=%s GIT_EMAIL=%s NET_USER=%s bash ~/.daybox-controlplane-setup.sh",
		shQuote(publicIP), shQuote(o.gitName), shQuote(o.gitEmail),
		shQuote(loadConfig().get("NET_USER", "dev")))
	if err := sshRun(control, setup); err != nil {
		log.Fatal("control-plane setup failed (output above)")
	}

	// register keys + workspace volume if the plane has a token
	if _, err := sshCapture(control, "test -f ~/.config/daybox/token"); err == nil {
		say("• registering ssh keys + workspace volume with Hetzner (daybox setup)")
		if err := sshRun(control, remoteDaybox+" setup"); err != nil {
			why := "the remote command failed (output above)"
			if sshTransient(err) {
				why = "the control plane was unreachable (transient ssh trouble) — the command never ran"
			}
			log.Fatalf("daybox setup did not complete: %s.\n"+
				"  the control plane IS up at %s (billing) — finish setup with:\n"+
				"    daybox init        (idempotent: adopts the existing plane, completes setup)\n"+
				"  or by hand:\n"+
				"    ssh %s '%s setup'", why, publicIP, control, remoteDaybox)
		}
	} else {
		say("  NOTE: no Hetzner token on %s yet — summoning needs one:", control)
		say("        ssh %s 'install -m 600 /dev/stdin ~/.config/daybox/token' < tokenfile; then: daybox status", control)
	}

	// ---- this device ----
	say("• writing ~/.config/daybox/config.local on this device")
	if err := writeLocalConfig([][2]string{
		{"CONTROL_HOST", control},
		{"LITTLEBOX_IP", publicIP},
		{"GIT_NAME", o.gitName},
		{"GIT_EMAIL", o.gitEmail},
	}); err != nil {
		log.Fatal(err)
	}

	if !o.noEnroll {
		say("")
		f := &netFlags{state: defaultStateDir(), hostname: o.device}
		enroll(loadConfig(), f)
	}

	say("")
	say("✓ done. Everyday use from here:")
	say("    daybox up        summon and ssh in (auto-reaps ~30min after you leave)")
	say("    daybox status    box, price, idle countdown")
	say("    daybox net       who's on your net")
}

func prompt(in *bufio.Reader, label, def string) string {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		if def != "" {
			return def
		}
		log.Fatalf("non-interactive and no flag for: %s", label)
	}
	if def != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", label)
	}
	line, _ := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// shQuote makes s safe to embed in a POSIX shell command line.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sanitizeDeviceName bends a proposed name (typically the machine's
// hostname) into one validDeviceName accepts: lowercased, every other rune
// becomes a dash, runs collapsed, dashes trimmed off the ends. Returns ""
// when nothing usable remains — the prompt then simply has no default.
func sanitizeDeviceName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	return out
}

// validDeviceName: the device name becomes a net hostname, a headscale node
// name and a keys/<name>.pub path component — keep it boring.
func validDeviceName(s string) bool {
	// no leading dash: these strings become argv words (ssh destinations,
	// path components) and must never be option-shaped.
	if s == "" || len(s) > 63 || s[0] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func gitDefault(key string) string {
	out, err := exec.Command("git", "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func isRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "remote", "controlplane-setup.sh"))
	return err == nil
}

// lookupRepo walks up from cwd looking for a checkout. Unlike findRepo it
// reports absence instead of exiting, so init can fall back to a release.
func lookupRepo() (string, bool) {
	dir, _ := os.Getwd()
	for d := dir; d != "/" && d != "."; d = filepath.Dir(d) {
		if isRepo(d) {
			return d, true
		}
	}
	return "", false
}

func findRepo(flagVal string) string {
	if flagVal != "" {
		if !isRepo(flagVal) {
			log.Fatalf("%s doesn't look like a daybox checkout", flagVal)
		}
		return flagVal
	}
	if dir, ok := lookupRepo(); ok {
		return dir
	}
	log.Fatal("not inside a daybox checkout — pass --repo /path/to/daybox")
	return ""
}

func findLocalPubkey() string {
	for _, name := range []string{"id_ed25519.pub", "id_rsa.pub", "id_ecdsa.pub"} {
		p := filepath.Join(os.Getenv("HOME"), ".ssh", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	log.Fatal("no ssh public key in ~/.ssh — create one: ssh-keygen -t ed25519")
	return ""
}

// scpTo copies a local file onto the control plane, consulting the same
// pinned known-hosts options as every other control-plane connection.
func scpTo(src, host, dst string) error {
	c := exec.Command("scp", append(sshOpts(true), "-q", src, host+":"+dst)...)
	c.Stdout, c.Stderr = os.Stderr, os.Stderr
	return c.Run()
}

// pushTree tars the checkout into ~/daybox on the control plane, leaving
// out .git and local build artifacts (dist/ alone is ~80MB). --no-xattrs:
// macOS stamps files written by a downloaded binary with a provenance xattr,
// bsdtar ships xattrs by default, and GNU tar on the far end then warns once
// per file — the metadata is meaningless off-mac, so drop it at the source.
func pushTree(repo, control string) error {
	// Retry the whole tar|ssh pair on a transport failure: a blip on the
	// unpack ssh breaks the pipe (tar then errors too), so both are rebuilt
	// per attempt — re-tarring is cheap and the unpack overwrites idempotently.
	return sshRetry("control plane", func() error {
		tar := exec.Command("tar", "-C", repo, "--no-xattrs",
			"--exclude=./.git", "--exclude=./dist", "--exclude=./cmd/daybox/daybox",
			"-czf", "-", ".")
		unpack := exec.Command("ssh", append(sshOpts(true), control,
			"mkdir -p ~/daybox && tar -xzf - -C ~/daybox")...)
		var err error
		unpack.Stdin, err = tar.StdoutPipe()
		if err != nil {
			return err
		}
		tar.Stderr, unpack.Stdout, unpack.Stderr = os.Stderr, os.Stderr, os.Stderr
		if err := tar.Start(); err != nil {
			return err
		}
		uerr := unpack.Run()
		twerr := tar.Wait()
		if uerr != nil {
			return uerr
		}
		return twerr
	})
}

// provisionControlPlane creates the VPS and a login user on it.
// Returns the ssh destination and the token (to store on the plane).
func provisionControlPlane(in *bufio.Reader, o *initOpts) (string, string) {
	token := readTokenFile(o.tokenFile)
	if token == "" {
		say("A Hetzner API token is needed (Console → project → Security → API tokens,")
		say("read/write, in a DEDICATED project — see SECURITY.md).")
		fmt.Fprint(os.Stderr, "paste token (hidden): ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil || len(b) == 0 {
			log.Fatal("no token")
		}
		token = strings.TrimSpace(string(b))
	}
	h := &hetzner{token: token}

	pub := findLocalPubkey()
	// Pin every subsequent control-plane ssh to this exact key: it's the one
	// we're registering on the box, so root user-setup can't be derailed by
	// an ssh-agent offering other keys first (MaxAuthTries) or by ssh's
	// default-identity resolution looking in the passwd home, not $HOME.
	sshIdentity = strings.TrimSuffix(pub, ".pub")
	keyName, err := h.ensureSSHKey(pub, o.device)
	if err != nil {
		log.Fatalf("registering ssh key: %v", err)
	}

	price := h.priceMonthly(o.serverType, o.location)
	if price == "" {
		price = "?"
	}
	say("• creating %s (%s, %s) — flat ~€%s/mo gross, billed by Hetzner to you", o.name, o.serverType, o.location, price)
	ip, existed, err := h.ensureServer(o.name, o.serverType, o.location, keyName)
	if err != nil {
		log.Fatalf("creating server: %v", err)
	}
	if existed {
		say("  server %q already exists at %s — adopting it", o.name, ip)
	} else {
		say("  created; %s — waiting for ssh", ip)
	}
	if err := waitTCP(ip+":22", 3*time.Minute); err != nil {
		log.Fatalf("ssh never came up on %s", ip)
	}

	// pin the fresh box's host keys into the dedicated file, replacing any
	// stale lines for this IP (Hetzner recycles addresses aggressively);
	// never touches ~/.ssh/known_hosts
	say("• pinning the box's host keys (%s)", controlKnownHosts())
	scan, err := exec.Command("ssh-keyscan", "-T", "5", ip).Output()
	if err != nil || len(scan) == 0 {
		log.Fatalf("ssh-keyscan of %s failed", ip)
	}
	var kept []string
	if b, err := os.ReadFile(controlKnownHosts()); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l != "" && !strings.HasPrefix(l, ip+" ") {
				kept = append(kept, l)
			}
		}
	}
	for _, l := range strings.Split(strings.TrimSpace(string(scan)), "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			kept = append(kept, l)
		}
	}
	os.MkdirAll(confDir(), 0o755)
	if err := os.WriteFile(controlKnownHosts(), []byte(strings.Join(kept, "\n")+"\n"), 0o600); err != nil {
		log.Fatal(err)
	}

	// first boot: only root exists; create the operator user
	root := "root@" + ip
	userSetup := fmt.Sprintf(`set -e
id %[1]s >/dev/null 2>&1 || adduser --disabled-password --gecos '' %[1]s
usermod -aG sudo %[1]s
echo '%[1]s ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/90-daybox
install -d -m 700 -o %[1]s -g %[1]s /home/%[1]s/.ssh
cp /root/.ssh/authorized_keys /home/%[1]s/.ssh/authorized_keys
chown %[1]s:%[1]s /home/%[1]s/.ssh/authorized_keys
chmod 600 /home/%[1]s/.ssh/authorized_keys`, o.user)
	// sshRun pins the identity (set above) + BatchMode, so this fails fast
	// instead of dropping to an interactive password prompt on auth trouble,
	// and rides out a transient connect blip on the fresh box.
	if err := sshRun(root, userSetup); err != nil {
		log.Fatalf("creating user on fresh VPS: %v", err)
	}
	return o.user + "@" + ip, token
}

func readTokenFile(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("token file: %v", err)
	}
	return strings.TrimSpace(string(b))
}
