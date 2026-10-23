package main

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

// cmdCompletion prints a completion script generated from the command table.
func cmdCompletion(args []string, stdout, stderr io.Writer) int {
	shells := map[string]func() string{"bash": bashCompletion, "zsh": zshCompletion, "fish": fishCompletion}
	if len(args) != 1 || shells[args[0]] == nil {
		fmt.Fprintln(stderr, "usage: prebake completion bash|zsh|fish")
		fmt.Fprintln(stderr, "  bash: source <(prebake completion bash)         (or save it under /etc/bash_completion.d/)")
		fmt.Fprintln(stderr, "  zsh:  prebake completion zsh > \"${fpath[1]}/_prebake\"")
		fmt.Fprintln(stderr, "  fish: prebake completion fish > ~/.config/fish/completions/prebake.fish")
		return 2
	}
	fmt.Fprint(stdout, shells[args[0]]())
	return 0
}

// cmdComplete is the hidden helper the scripts call for names that only the running daemon
// knows. It never fails loudly: a completion that prints an error is worse than none.
func cmdComplete(args []string, stdout io.Writer) int {
	if len(args) == 0 || (args[0] != "containers" && args[0] != "orphans") {
		return 0
	}
	what := args[0]
	fs := flag.NewFlagSet("__complete", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	url := clientFlags(fs)
	fs.Parse(args[1:])
	dto, err := fetchStatus(*url)
	if err != nil {
		return 0
	}
	seen := map[string]bool{}
	var names []string
	add := func(container, flavor string) {
		for _, n := range []string{container, container + ":" + flavor} {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	if what == "orphans" { // what `purge` can remove: images of containers that are no longer in the config
		for _, o := range dto.Orphans {
			add(o.Container, o.Flavor)
		}
	} else {
		for _, j := range dto.Jobs {
			add(j.Container, j.Flavor)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintln(stdout, n)
	}
	return 0
}

func (f flagSpec) dashed() []string {
	out := []string{"--" + f.Name}
	if f.Short != "" {
		out = append(out, "-"+f.Short)
	}
	return out
}

func (f flagSpec) enum() []string {
	if strings.Contains(f.Value, "|") {
		return strings.Split(f.Value, "|")
	}
	return nil
}

func commandNames() []string {
	var n []string
	for _, c := range commands {
		n = append(n, c.Name)
	}
	return n
}

// --- bash ---------------------------------------------------------------------------------

func bashCompletion() string {
	var b strings.Builder
	b.WriteString("# bash completion for prebake: source <(prebake completion bash)\n")
	b.WriteString(`_prebake() {
  # the word under the cursor, including ':' (bash splits words at ':' by default, which
  # would cut container:flavor in two)
  local cur=${COMP_LINE:0:COMP_POINT}
  cur=${cur##* }
  local prefix=${cur%"${cur##*:}"}
  local cmd=${COMP_WORDS[1]} prev=${COMP_WORDS[COMP_CWORD-1]} words="" valued=0
  COMPREPLY=()
  if [ "$COMP_CWORD" -le 1 ]; then
    words="` + strings.Join(commandNames(), " ") + `"
  else
    case "$cmd" in
`)
	for _, c := range commands {
		var fl []string
		for _, f := range c.Flags {
			fl = append(fl, f.dashed()...)
		}
		fmt.Fprintf(&b, "      %s) words=%q ;;\n", c.Name, strings.Join(fl, " "))
	}
	b.WriteString("    esac\n    case \"$cmd:$prev\" in\n")
	for _, c := range commands {
		for _, f := range c.Flags {
			for _, d := range f.dashed() {
				switch {
				case f.enum() != nil:
					fmt.Fprintf(&b, "      %s:%s) words=%q; valued=1 ;;\n", c.Name, d, strings.Join(f.enum(), " "))
				case f.Value == "dir":
					fmt.Fprintf(&b, "      %s:%s) COMPREPLY=($(compgen -d -- \"$cur\")); return ;;\n", c.Name, d)
				case f.Value == "file":
					fmt.Fprintf(&b, "      %s:%s) COMPREPLY=($(compgen -f -- \"$cur\")); return ;;\n", c.Name, d)
				case f.Value != "":
					fmt.Fprintf(&b, "      %s:%s) return ;;\n", c.Name, d) // free text: nothing to offer
				}
			}
		}
	}
	b.WriteString("    esac\n    case \"$cmd\" in\n")
	for _, c := range commands {
		switch c.Args {
		case "containers", "orphans":
			fmt.Fprintf(&b, "      %s) [[ \"$cur\" == -* || $valued == 1 ]] || words=\"$words $(prebake __complete %s 2>/dev/null)\" ;;\n", c.Name, c.Args)
		case "shell":
			fmt.Fprintf(&b, "      %s) words=\"bash zsh fish\" ;;\n", c.Name)
		}
	}
	b.WriteString(`    esac
  fi
  COMPREPLY=($(compgen -W "$words" -- "$cur"))
  # bash replaces only what follows the last ':' of the word
  local i
  for i in "${!COMPREPLY[@]}"; do COMPREPLY[$i]=${COMPREPLY[$i]#"$prefix"}; done
}
complete -F _prebake prebake
`)
	return b.String()
}

// --- zsh ----------------------------------------------------------------------------------

func zshEscape(s string) string {
	return strings.NewReplacer("'", "'\\''", "[", "\\[", "]", "\\]", ":", "\\:").Replace(s)
}

func zshCompletion() string {
	var b strings.Builder
	b.WriteString("#compdef prebake\n# zsh completion for prebake: prebake completion zsh > \"${fpath[1]}/_prebake\"\n\n")
	b.WriteString("_prebake_containers() {\n  local -a c\n  c=(${${(f)\"$(prebake __complete containers 2>/dev/null)\"}//:/\\\\:})\n  _describe 'container' c\n}\n\n")
	b.WriteString("_prebake_orphans() {\n  local -a c\n  c=(${${(f)\"$(prebake __complete orphans 2>/dev/null)\"}//:/\\\\:})\n  _describe 'removed container' c\n}\n\n")
	b.WriteString("_prebake() {\n  local -a cmds\n  cmds=(\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "    '%s:%s'\n", c.Name, zshEscape(c.Help))
	}
	b.WriteString("  )\n  if (( CURRENT == 2 )); then\n    _describe 'command' cmds\n    return\n  fi\n  case $words[2] in\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "    %s)\n      _arguments \\\n", c.Name)
		for _, f := range c.Flags {
			action := ""
			switch {
			case f.enum() != nil:
				action = ":value:(" + strings.Join(f.enum(), " ") + ")"
			case f.Value == "dir":
				action = ":directory:_directories"
			case f.Value == "file":
				action = ":file:_files"
			case f.Value != "":
				action = ":value: "
			}
			help := zshEscape(f.Help)
			if f.Short != "" {
				fmt.Fprintf(&b, "        '(-%s --%s)'{-%s,--%s}'[%s]%s' \\\n", f.Short, f.Name, f.Short, f.Name, help, action)
			} else {
				fmt.Fprintf(&b, "        '--%s[%s]%s' \\\n", f.Name, help, action)
			}
		}
		switch c.Args {
		case "containers":
			b.WriteString("        '1:container:_prebake_containers'\n")
		case "orphans":
			b.WriteString("        '1:removed container:_prebake_orphans'\n")
		case "shell":
			b.WriteString("        '1:shell:(bash zsh fish)'\n")
		default:
			b.WriteString("        '-h[show help]'\n")
		}
		b.WriteString("      ;;\n")
	}
	b.WriteString("  esac\n}\n\n_prebake \"$@\"\n")
	return b.String()
}

// --- fish ---------------------------------------------------------------------------------

func fishEscape(s string) string { return strings.NewReplacer("\\", "\\\\", "'", "\\'").Replace(s) }

func fishCompletion() string {
	var b strings.Builder
	b.WriteString("# fish completion for prebake: prebake completion fish > ~/.config/fish/completions/prebake.fish\n")
	b.WriteString("complete -c prebake -f\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "complete -c prebake -n '__fish_use_subcommand' -a '%s' -d '%s'\n", c.Name, fishEscape(c.Help))
	}
	for _, c := range commands {
		cond := fmt.Sprintf("-n '__fish_seen_subcommand_from %s'", c.Name)
		for _, f := range c.Flags {
			line := fmt.Sprintf("complete -c prebake %s -l %s", cond, f.Name)
			if f.Short != "" {
				line += " -s " + f.Short
			}
			switch {
			case f.enum() != nil:
				line += " -x -a '" + strings.Join(f.enum(), " ") + "'"
			case f.Value == "dir":
				line += " -r -a '(__fish_complete_directories)'"
			case f.Value == "file":
				line += " -r -F"
			case f.Value != "":
				line += " -x"
			}
			fmt.Fprintf(&b, "%s -d '%s'\n", line, fishEscape(f.Help))
		}
		switch c.Args {
		case "containers", "orphans":
			fmt.Fprintf(&b, "complete -c prebake %s -a '(prebake __complete %s 2>/dev/null)' -d 'container[:flavor]'\n", cond, c.Args)
		case "shell":
			fmt.Fprintf(&b, "complete -c prebake %s -a 'bash zsh fish'\n", cond)
		}
	}
	return b.String()
}
