package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// render.go — the Go port of bash render_user_data. Expands the cloud-init
// template's @REPO:/@FILE:/@SSH_KEYS@/@PROFILE_SEED@ includes (indentation
// preserved, comments stripped — TOML-multiline-string-aware for the seed)
// and substitutes @VOLUME_ID@/@REMOTE_USER@/@GIT_NAME@/@GIT_EMAIL@/
// @LITTLEBOX_IP@ placeholders.
//
// The cloud-init template lives at <repo>/cloud-init/cloud-init.yaml.template
// and is read at runtime (bash: TEMPLATE="$REPO_DIR/cloud-init/..."), so a
// template change takes effect on the next render without a rebuild.

// cloudInitTemplate is the path to the cloud-init template (bash: TEMPLATE).
func cloudInitTemplate(d *deployment) string {
	return filepath.Join(d.repoDir, "cloud-init", "cloud-init.yaml.template")
}

// buildAuthkeysSnippet emits every *.pub in dir as YAML list items (bash:
// build_authkeys_snippet). Each line is "- <keyline>" with no leading
// indent — the renderer's emit() adds the directive line's indent.
func buildAuthkeysSnippet(keysDir string) (string, error) {
	entries, err := filepath.Glob(filepath.Join(keysDir, "*.pub"))
	if err != nil {
		return "", fmt.Errorf("glob %s: %w", keysDir, err)
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("no authorized pubkeys in %s", keysDir)
	}
	var out strings.Builder
	for _, f := range entries {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", f, err)
		}
		// tr -d '\n' < "$f"  — the keyline, newlines stripped
		line := strings.ReplaceAll(string(b), "\n", "")
		line = strings.ReplaceAll(line, "\r", "")
		fmt.Fprintf(&out, "- %s\n", line)
	}
	return out.String(), nil
}

// renderUserData is the faithful port of bash render_user_data($1=vid).
// It does NOT write the authkeys snippet to a temp file (bash did, then awk
// read it back); the snippet is passed straight into emit.
func renderUserData(p *profile, volumeID string) (string, error) {
	// These values land inside a shell script (firstboot) and a heredoc; a
	// newline is the one character no quoting below can contain. Everything
	// else is made safe by LITERAL substitution (bash ${var//…}, not sed).
	for _, v := range []string{p.gitName, p.gitEmail, p.remoteUser} {
		if strings.ContainsAny(v, "\n\r") {
			return "", fmt.Errorf("GIT_NAME/GIT_EMAIL/REMOTE_USER must be single-line values")
		}
	}
	snippet, err := buildAuthkeysSnippet(p.dep.keysDir())
	if err != nil {
		return "", err
	}
	tmpl, err := os.ReadFile(cloudInitTemplate(p.dep))
	if err != nil {
		return "", fmt.Errorf("read cloud-init template: %w", err)
	}
	seed, err := os.ReadFile(p.seedFile())
	if err != nil {
		return "", fmt.Errorf("read profile seed: %w", err)
	}
	var out bytes.Buffer
	for _, line := range splitLines(string(tmpl)) {
		indent := leadingSpaces(line)
		switch {
		case strings.Contains(line, "@REPO:"):
			path := extractDirective(line, "@REPO:")
			content, err := os.ReadFile(filepath.Join(p.dep.repoDir, path))
			if err != nil {
				return "", fmt.Errorf("include @REPO:%s: %w", path, err)
			}
			emitInclude(&out, indent, string(content), false)
		case strings.Contains(line, "@FILE:"):
			path := extractDirective(line, "@FILE:")
			content, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("include @FILE:%s: %w", path, err)
			}
			emitInclude(&out, indent, string(content), false)
		case strings.Contains(line, "@SSH_KEYS@"):
			emitInclude(&out, indent, snippet, false)
		case strings.Contains(line, "@PROFILE_SEED@"):
			emitInclude(&out, indent, string(seed), true)
		default:
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	rendered := out.String()
	// Placeholder substitution (bash: ${rendered//@TOKEN@/"$val"}). Quoted so
	// patsub_replacement can't reinterpret & in the value.
	rendered = strings.ReplaceAll(rendered, "@VOLUME_ID@", volumeID)
	rendered = strings.ReplaceAll(rendered, "@REMOTE_USER@", p.remoteUser)
	rendered = strings.ReplaceAll(rendered, "@GIT_NAME@", p.gitName)
	rendered = strings.ReplaceAll(rendered, "@GIT_EMAIL@", p.gitEmail)
	rendered = strings.ReplaceAll(rendered, "@LITTLEBOX_IP@", p.littleboxIP)
	return rendered, nil
}

// emitInclude is the port of awk emit(path, toml). It writes each line of
// content with `indent` prepended, applying comment stripping:
//   - non-toml (REPO/FILE/SSH_KEYS): a full-line comment is dropped, EXCEPT
//     a shebang on the FIRST included line survives (so scripts keep #!).
//   - toml (PROFILE_SEED): a full-line comment is dropped ONLY when outside a
//     TOML multiline string. A #-line inside a """ / ''' block is content —
//     stripping it would make the box run a different command than the
//     profile declares, the exact silent drift the seed contract forbids.
//
// The in-string state toggles on every """ and ''' occurrence in a line, but
// a comment line OUTSIDE a string does not toggle (a comment may quote
// delimiters in prose without affecting string state). awk: toggles().
func emitInclude(out *bytes.Buffer, indent, content string, toml bool) {
	lines := splitLines(content)
	inStr := false
	for i, line := range lines {
		first := i == 0
		isComment := strings.HasPrefix(stripLeadingWS(line), "#")
		isShebang := strings.HasPrefix(line, "#!")
		// print condition (awk): (toml && instr) || !comment || (!toml && first && shebang)
		if (toml && inStr) || !isComment || (!toml && first && isShebang) {
			out.WriteString(indent)
			out.WriteString(line)
			out.WriteByte('\n')
		}
		// toggle inStr (toml only), UNLESS currently out-of-string AND a comment
		if toml && !(inStr == false && isComment) {
			inStr = toggles(line, inStr)
		}
	}
}

// toggles counts """ and ''' occurrences in line and flips inStr if odd.
// awk: gsub(/"""/,"",t) + gsub(sq sq sq,"",t); return (instr + n) % 2.
func toggles(line string, inStr bool) bool {
	n := strings.Count(line, `"""`) + strings.Count(line, `'''`)
	if n%2 == 1 {
		return !inStr
	}
	return inStr
}

// extractDirective pulls the path out of "<indent>@REPO:path@" (bash:
// sub(/^ *@REPO:/,"",path); sub(/@[[:space:]]*$/,"",path)). The path is
// between @REPO: and the trailing @ (plus optional trailing whitespace).
func extractDirective(line, marker string) string {
	s := line
	// strip leading whitespace + marker from the front
	idx := strings.Index(s, marker)
	if idx >= 0 {
		s = s[idx+len(marker):]
	}
	// strip trailing @ + optional whitespace
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, " \t")
}

// leadingSpaces returns the leading 0x20-space prefix (awk /^ */).
func leadingSpaces(s string) string {
	i := 0
	for i < len(s) && s[i] == ' ' {
		i++
	}
	return s[:i]
}

func stripLeadingWS(s string) string {
	return strings.TrimLeft(s, " \t")
}

// splitLines splits on \n and drops a single trailing empty element (from a
// final newline), matching awk's getline which stops at EOF without
// producing a phantom blank line.
func splitLines(s string) []string {
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
