package main

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Minimal Hetzner Cloud client — just what `init --provision` needs.
// The full provider abstraction (TODO track 2) will subsume this.

type hetzner struct{ token string }

func (h *hetzner) req(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "https://api.hetzner.cloud/v1"+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("hetzner %s %s: %s: %s", method, path, resp.Status, b)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// md5Fingerprint computes the colon-separated MD5 fingerprint Hetzner uses
// to identify ssh keys.
func md5Fingerprint(pubkeyPath string) (string, error) {
	b, err := os.ReadFile(pubkeyPath)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		return "", fmt.Errorf("%s doesn't look like an ssh public key", pubkeyPath)
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "", err
	}
	sum := md5.Sum(blob)
	hexstr := hex.EncodeToString(sum[:])
	var parts []string
	for i := 0; i < len(hexstr); i += 2 {
		parts = append(parts, hexstr[i:i+2])
	}
	return strings.Join(parts, ":"), nil
}

// ensureSSHKey registers the pubkey if its fingerprint is unknown; returns
// the Hetzner-side key name to reference at server creation.
func (h *hetzner) ensureSSHKey(pubkeyPath, name string) (string, error) {
	fp, err := md5Fingerprint(pubkeyPath)
	if err != nil {
		return "", err
	}
	var got struct {
		Keys []struct {
			Name string `json:"name"`
		} `json:"ssh_keys"`
	}
	if err := h.req("GET", "/ssh_keys?fingerprint="+fp, nil, &got); err != nil {
		return "", err
	}
	if len(got.Keys) > 0 {
		return got.Keys[0].Name, nil
	}
	b, _ := os.ReadFile(pubkeyPath)
	var created struct {
		Key struct {
			Name string `json:"name"`
		} `json:"ssh_key"`
	}
	err = h.req("POST", "/ssh_keys", map[string]string{
		"name": name, "public_key": strings.TrimSpace(string(b)),
	}, &created)
	return created.Key.Name, err
}

type hzServer struct {
	ID        int64  `json:"id"`
	Status    string `json:"status"`
	PublicNet struct {
		IPv4 struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
}

// ensureServer creates the VPS (or finds one with that name); returns its
// IPv4 and whether it already existed.
func (h *hetzner) ensureServer(name, serverType, location, sshKey string) (string, bool, error) {
	var list struct {
		Servers []hzServer `json:"servers"`
	}
	if err := h.req("GET", "/servers?name="+name, nil, &list); err != nil {
		return "", false, err
	}
	if len(list.Servers) > 0 {
		return list.Servers[0].PublicNet.IPv4.IP, true, nil
	}
	var created struct {
		Server hzServer `json:"server"`
	}
	err := h.req("POST", "/servers", map[string]any{
		"name": name, "server_type": serverType, "image": "ubuntu-24.04",
		"location": location, "ssh_keys": []string{sshKey},
		"labels": map[string]string{"role": "daybox-control"},
	}, &created)
	if err != nil {
		return "", false, err
	}
	// wait for running
	for i := 0; i < 60; i++ {
		var one struct {
			Server hzServer `json:"server"`
		}
		if err := h.req("GET", fmt.Sprintf("/servers/%d", created.Server.ID), nil, &one); err == nil &&
			one.Server.Status == "running" {
			return one.Server.PublicNet.IPv4.IP, false, nil
		}
		time.Sleep(2 * time.Second)
	}
	return created.Server.PublicNet.IPv4.IP, false, fmt.Errorf("server never reached running")
}

// priceMonthly returns the gross monthly price ("17.49") for a server type
// at a location, or "" when the lookup fails — callers print "?" then. Never
// hardcode a price in user-facing text; Hetzner's differ per location and
// change over time (the old "~€4/mo" copy was off by 4x in US locations).
func (h *hetzner) priceMonthly(serverType, location string) string {
	var got struct {
		Types []struct {
			Prices []struct {
				Location string `json:"location"`
				Monthly  struct {
					Gross string `json:"gross"`
				} `json:"price_monthly"`
			} `json:"prices"`
		} `json:"server_types"`
	}
	if err := h.req("GET", "/server_types?name="+serverType, nil, &got); err != nil || len(got.Types) == 0 {
		return ""
	}
	for _, p := range got.Types[0].Prices {
		if p.Location == location {
			if f, err := strconv.ParseFloat(p.Monthly.Gross, 64); err == nil {
				return strconv.FormatFloat(f, 'f', 2, 64)
			}
			return p.Monthly.Gross
		}
	}
	return ""
}

func waitTCP(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout")
}
