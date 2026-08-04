package main

// Box-proposed profile changes — the laptop review half (TODO §1e, P3).
//
// A proposal is a FULL profile.toml rewrite staged on the control plane at
// ~/.config/daybox/profiles/<name>/proposals/<id>.toml (the relay, P4,
// writes them; a box can propose, never approve). Review is a unified diff
// of live vs proposed — the diff shows *everything* the box wants, and
// [setup]/[persist] lines are flagged loudly because they are the
// supply-chain-bearing surface ([setup] once runs verbatim as commands).
// Approval is a laptop-side action: `accept` re-validates, shows the diff,
// confirms, then replaces the live seed (with the same backup discipline as
// `profile edit`). The control plane stores; it never approves.

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

type proposal struct{ profile, id string }

// validProposalID keeps ids path- and shell-safe; they are filenames minted
// by the relay, but the laptop must not trust what it lists off the plane.
func validProposalID(id string) bool {
	if id == "" || strings.HasPrefix(id, ".") {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_') {
			return false
		}
	}
	return true
}

func remoteProposalPath(p proposal) string {
	return `"$HOME"/.config/daybox/profiles/` + shq(p.profile) + `/proposals/` + shq(p.id+".toml")
}

// listProposals enumerates every pending proposal on the control plane,
// skipping (and warning about) entries whose profile or id would not be
// safe to embed in a shell command.
func listProposals(host string) ([]proposal, error) {
	out, err := sshCapture(host,
		`ls -1 "$HOME"/.config/daybox/profiles/*/proposals/*.toml 2>/dev/null || true`)
	if err != nil {
		return nil, err
	}
	var ps []proposal
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "/")
		if len(parts) < 3 {
			continue
		}
		id := strings.TrimSuffix(parts[len(parts)-1], ".toml")
		prof := parts[len(parts)-3]
		if !validProfileName(prof) || !validProposalID(id) {
			say("ignoring oddly-named proposal: %s", line)
			continue
		}
		ps = append(ps, proposal{profile: prof, id: id})
	}
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].profile != ps[j].profile {
			return ps[i].profile < ps[j].profile
		}
		return ps[i].id < ps[j].id
	})
	return ps, nil
}

// findProposal resolves an id (unique by construction: the relay mints
// timestamped names) to its profile.
func findProposal(host, id string) (proposal, error) {
	ps, err := listProposals(host)
	if err != nil {
		return proposal{}, err
	}
	for _, p := range ps {
		if p.id == id {
			return p, nil
		}
	}
	return proposal{}, fmt.Errorf("no pending proposal '%s' — see: daybox profile proposals", id)
}

// --- diff -------------------------------------------------------------------

type diffOp struct {
	op   byte // ' ' context, '-' removed, '+' added
	text string
}

// diffLines is a plain LCS line diff — a profile is ~100 lines, so the
// simple quadratic table beats carrying a diff dependency.
func diffLines(a, b []string) []diffOp {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

// supplyChainFlag marks changed lines in the [setup]/[persist] sections —
// the surface where a proposal can smuggle a verbatim-run command or move a
// path onto the volume. The backstop for full-rewrite proposals: dangerous
// changes are possible but impossible to skim past.
const supplyChainFlag = "   ⚠ supply-chain surface"

// renderProposalDiff produces the review diff: changed lines with 2 lines
// of context, long unchanged runs collapsed, [setup]/[persist] changes
// flagged. Section state tracks across context and changed lines alike.
func renderProposalDiff(current, proposed string) string {
	ops := diffLines(strings.Split(current, "\n"), strings.Split(proposed, "\n"))

	// keep indices near any change: the change itself ± 2 context lines
	keep := make([]bool, len(ops))
	for i, o := range ops {
		if o.op == ' ' {
			continue
		}
		for k := i - 2; k <= i+2; k++ {
			if k >= 0 && k < len(ops) {
				keep[k] = true
			}
		}
	}

	var b strings.Builder
	section, elided := "", false
	for i, o := range ops {
		t := strings.TrimSpace(o.text)
		if strings.HasPrefix(t, "[") {
			section = strings.TrimLeft(t, "[")
			for _, cut := range []string{"]", "."} {
				if k := strings.Index(section, cut); k >= 0 {
					section = section[:k]
				}
			}
		}
		if !keep[i] {
			if !elided {
				b.WriteString("  …\n")
				elided = true
			}
			continue
		}
		elided = false
		flag := ""
		if o.op != ' ' && (section == "setup" || section == "persist") {
			flag = supplyChainFlag
		}
		fmt.Fprintf(&b, "%c %s%s\n", o.op, o.text, flag)
	}
	return b.String()
}

// --- verbs ------------------------------------------------------------------

// cmdProfileProposals: list every pending proposal with its review diff.
func cmdProfileProposals(args []string) {
	host := mustControl()
	ps, err := listProposals(host)
	if err != nil {
		log.Fatal(err)
	}
	if len(ps) == 0 {
		say("no pending proposals")
		return
	}
	for _, p := range ps {
		cur, err := fetchProfile(host, p.profile)
		if err != nil {
			log.Fatal(err)
		}
		prop, err := sshCapture(host, "cat "+remoteProposalPath(p))
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s → profile '%s'\n", p.id, p.profile)
		fmt.Print(renderProposalDiff(cur, prop))
		fmt.Println("accept: daybox profile accept " + p.id +
			"   reject: daybox profile reject " + p.id)
	}
}

// cmdProfileAccept: review a proposal and, on confirmation, make it the
// live profile. The content that goes live is the exact bytes reviewed —
// pushed from here, not moved on the plane — so a proposal re-submitted
// mid-review can't swap in something the diff never showed.
func cmdProfileAccept(args []string) {
	if len(args) != 1 {
		log.Fatal("usage: daybox profile accept <id>")
	}
	host := mustControl()
	p, err := findProposal(host, args[0])
	if err != nil {
		log.Fatal(err)
	}
	cur, err := fetchProfile(host, p.profile)
	if err != nil {
		log.Fatal(err)
	}
	prop, err := sshCapture(host, "cat "+remoteProposalPath(p))
	if err != nil {
		log.Fatal(err)
	}
	if prop == cur {
		say("proposal %s matches the live profile — nothing to apply", p.id)
		rmProposal(host, p)
		return
	}
	// a box could submit anything; the same validator that gates an edit
	// gates an acceptance
	if err := validateProfile(prop); err != nil {
		log.Fatalf("proposal %s is not a valid profile (%v) — reject it: daybox profile reject %s",
			p.id, err, p.id)
	}
	fmt.Printf("%s → profile '%s'\n", p.id, p.profile)
	fmt.Print(renderProposalDiff(cur, prop))
	in := bufio.NewReader(os.Stdin)
	if !strings.HasPrefix(strings.ToLower(prompt(in, "replace live profile '"+p.profile+"'?", "n")), "y") {
		say("not applied — proposal %s stays pending", p.id)
		return
	}
	ts := time.Now().Format("20060102-150405")
	if err := pushProfile(host, p.profile, prop, ts); err != nil {
		log.Fatal(err)
	}
	rmProposal(host, p)
	say("profile '%s' updated (backup: profile.toml.bak.%s) — takes effect at the next daybox up", p.profile, ts)
}

// cmdProfileReject drops a pending proposal.
func cmdProfileReject(args []string) {
	if len(args) != 1 {
		log.Fatal("usage: daybox profile reject <id>")
	}
	host := mustControl()
	p, err := findProposal(host, args[0])
	if err != nil {
		log.Fatal(err)
	}
	rmProposal(host, p)
	say("rejected proposal %s (profile '%s' unchanged)", p.id, p.profile)
}

func rmProposal(host string, p proposal) {
	if err := sshRun(host, "rm -f "+remoteProposalPath(p)); err != nil {
		log.Fatalf("could not remove proposal %s: %v", p.id, err)
	}
}

// maybeOfferProposalReview is `daybox up`'s pre-summon hook (§1e P6): when
// the profile about to be rendered has pending proposals, offer a review —
// without ever blocking the summon. Timeout, 'n', a non-tty stdin, or any
// error here all mean "summon with the profile as it stands".
func maybeOfferProposalReview(host, profileArg string) {
	ps, err := listProposals(host)
	if err != nil || len(ps) == 0 {
		return
	}
	name := profileArg
	if name == "" {
		name, _ = remoteDefaultProfile(host) // "" on error: fall back to all
	}
	var mine []proposal
	for _, p := range ps {
		if name == "" || p.profile == name {
			mine = append(mine, p)
		}
	}
	if len(mine) == 0 {
		return
	}
	say("%d proposal(s) pending for '%s':", len(mine), name)
	for _, p := range mine {
		say("  %s", p.id)
	}
	if !isTTY() {
		say("summoning with the current profile — review: daybox profile proposals")
		return
	}
	fmt.Fprintf(os.Stderr, "review now? [y/N] — continuing in 5s: ")
	// poll rather than read-with-deadline: Go leaves std fds in blocking
	// mode (SetReadDeadline fails on a real tty), and a leaked blocked
	// reader would steal the first line the user types into the box's
	// shell after the summon. Polling consumes nothing on timeout.
	if !stdinReadable(5 * time.Second) {
		fmt.Fprintln(os.Stderr)
		say("continuing — review later: daybox profile proposals")
		return
	}
	in := bufio.NewReader(os.Stdin)
	line, err := in.ReadString('\n')
	if err != nil {
		fmt.Fprintln(os.Stderr)
		say("continuing — review later: daybox profile proposals")
		return
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
		reviewProposals(host, mine, in)
	}
}

// stdinReadable reports whether a line is waiting on stdin within d.
func stdinReadable(d time.Duration) bool {
	fds := []unix.PollFd{{Fd: int32(os.Stdin.Fd()), Events: unix.POLLIN}}
	for deadline := time.Now().Add(d); ; {
		left := time.Until(deadline)
		if left < 0 {
			return false
		}
		n, err := unix.Poll(fds, int(left.Milliseconds()))
		if err == unix.EINTR {
			continue
		}
		return err == nil && n > 0
	}
}

// reviewProposals walks pending proposals one by one: diff, then
// accept / reject / skip. Accepted ones replace the live profile NOW —
// before the caller renders — so they apply to the box being summoned.
func reviewProposals(host string, ps []proposal, in *bufio.Reader) {
	for _, p := range ps {
		cur, err := fetchProfile(host, p.profile)
		if err != nil {
			say("%s: %v — skipping", p.id, err)
			continue
		}
		prop, err := sshCapture(host, "cat "+remoteProposalPath(p))
		if err != nil {
			say("%s: unreadable — skipping", p.id)
			continue
		}
		if prop == cur {
			say("%s matches the live profile — dropping it", p.id)
			rmProposal(host, p)
			continue
		}
		fmt.Printf("%s → profile '%s'\n", p.id, p.profile)
		if err := validateProfile(prop); err != nil {
			say("NOT a valid profile (%v)", err)
			if strings.HasPrefix(strings.ToLower(prompt(in, "reject it?", "y")), "y") {
				rmProposal(host, p)
				say("rejected %s", p.id)
			}
			continue
		}
		fmt.Print(renderProposalDiff(cur, prop))
		switch strings.ToLower(prompt(in, "[a]ccept / [r]eject / [s]kip", "s")) {
		case "a", "accept":
			ts := time.Now().Format("20060102-150405")
			if err := pushProfile(host, p.profile, prop, ts); err != nil {
				log.Fatal(err)
			}
			rmProposal(host, p)
			say("accepted %s (backup: profile.toml.bak.%s) — applies to this summon", p.id, ts)
		case "r", "reject":
			rmProposal(host, p)
			say("rejected %s", p.id)
		default:
			say("skipped %s — it stays pending", p.id)
		}
	}
}

func isTTY() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
