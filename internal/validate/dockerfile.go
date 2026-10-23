package validate

import (
	"encoding/json"
	"strings"
)

// instruction is one logical Dockerfile instruction (continuations joined).
type instruction struct {
	Cmd  string // upper-case, e.g. "COPY"
	Args string // raw argument string
	Line int    // 1-based line of the first physical line
}

// parseDockerfile splits a Dockerfile into instructions. It understands comments,
// line continuations (backslash) and ignores heredoc bodies.
func parseDockerfile(src string) []instruction {
	var out []instruction
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var cur strings.Builder
	start := 0
	flush := func() {
		text := strings.TrimSpace(cur.String())
		cur.Reset()
		if text == "" {
			return
		}
		cmd, args, _ := strings.Cut(text, " ")
		out = append(out, instruction{Cmd: strings.ToUpper(cmd), Args: strings.TrimSpace(args), Line: start})
	}
	var heredoc string
	for i, raw := range lines {
		if heredoc != "" {
			if strings.TrimSpace(raw) == heredoc {
				heredoc = ""
			}
			continue
		}
		trim := strings.TrimSpace(raw)
		if cur.Len() == 0 {
			if trim == "" || strings.HasPrefix(trim, "#") {
				continue
			}
			start = i + 1
		} else if strings.HasPrefix(trim, "#") {
			continue // comment inside a continuation
		}
		if strings.HasSuffix(trim, "\\") {
			cur.WriteString(strings.TrimSuffix(trim, "\\"))
			cur.WriteByte(' ')
			continue
		}
		cur.WriteString(trim)
		if h := heredocMarker(cur.String()); h != "" {
			heredoc = h
		}
		flush()
	}
	flush()
	return out
}

// heredocMarker returns the terminator word if the instruction opens a heredoc.
func heredocMarker(s string) string {
	_, rest, ok := strings.Cut(s, "<<")
	if !ok {
		return ""
	}
	rest = strings.TrimLeft(rest, "-")
	f := strings.Fields(rest)
	if len(f) == 0 {
		return ""
	}
	return strings.Trim(f[0], `"'`)
}

// copySources returns the source operands of a COPY/ADD instruction and
// whether it copies from another build stage/image (--from).
func copySources(args string) (sources []string, fromStage bool) {
	args = strings.TrimSpace(args)
	var fields []string
	if strings.HasPrefix(args, "[") {
		if err := json.Unmarshal([]byte(args), &fields); err != nil {
			fields = nil
		}
	}
	if fields == nil {
		fields = strings.Fields(args)
	}
	var operands []string
	for _, f := range fields {
		if strings.HasPrefix(f, "--") && len(operands) == 0 {
			if strings.HasPrefix(f, "--from=") {
				fromStage = true
			}
			continue
		}
		operands = append(operands, f)
	}
	if len(operands) < 2 {
		return nil, fromStage
	}
	return operands[:len(operands)-1], fromStage
}
