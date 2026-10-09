package main

// `sboot completion zsh|bash|fish|powershell` (DF-5, 2026-10-07): a hand-written
// completion script, generated from commandTable — no library, because
// harness/go.mod keeps zero requires. The table does not gate anything — the
// dispatcher's own `default:` refuses an unknown command. What keeps the two
// from drifting is completion_test.go: it holds the table's names equal to the
// dispatcher's `case` labels (less the `update` alias, which is not offered) and
// each command's flags equal to the per-command guards in parseCommon (G665, G670).

import (
	"fmt"
	"io"
	"os"
	"strings"
)

type commandSpec struct {
	name  string
	flags []string // the flags parseCommon accepts for it, besides --help/--no-color
}

// commandTable is every command the dispatcher answers, in help order.
var commandTable = []commandSpec{
	{"start", []string{"--dir", "--yes"}},
	{"test", []string{"--json", "--checks"}},
	{"hint", nil},
	{"explain", []string{"--here", "--message"}},
	{"submit", []string{"--force", "--json"}},
	{"courses", nil},
	{"login", nil},
	{"repo", []string{"--name", "--yes"}},
	{"resume", nil},
	{"reveal", []string{"--yes"}},
	{"logout", nil},
	{"whoami", nil},
	{"debug", nil},
	{"fetch", nil},
	{"where", nil},
	{"upgrade", []string{"--force"}},
	{"completion", nil},
	{"version", nil},
	{"help", []string{"--all"}},
}

var completionShells = []string{"zsh", "bash", "fish", "powershell"}

func commandNames() []string {
	var out []string
	for _, c := range commandTable {
		out = append(out, c.name)
	}
	return out
}

func flagsFor(c commandSpec) []string {
	return append(append([]string{}, c.flags...), "--help", "--no-color")
}

func runCompletion(args []string) int {
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: sboot completion %s\n", strings.Join(completionShells, "|"))
		return 2
	}
	if err := writeCompletion(os.Stdout, args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "sboot: %v\n", err)
		return 2
	}
	return 0
}

func writeCompletion(w io.Writer, shell string) error {
	cmds := strings.Join(commandNames(), " ")
	switch shell {
	case "bash":
		fmt.Fprintf(w, "# sboot completion for bash — add to ~/.bashrc:  source <(sboot completion bash)\n_sboot() {\n  local cur=\"${COMP_WORDS[COMP_CWORD]}\"\n  if [ \"$COMP_CWORD\" -eq 1 ]; then\n    COMPREPLY=($(compgen -W \"%s\" -- \"$cur\")); return\n  fi\n  case \"${COMP_WORDS[1]}\" in\n", cmds)
		for _, c := range commandTable {
			words := strings.Join(flagsFor(c), " ")
			if c.name == "completion" {
				words = strings.Join(completionShells, " ")
			}
			fmt.Fprintf(w, "    %s) COMPREPLY=($(compgen -W \"%s\" -- \"$cur\")) ;;\n", c.name, words)
		}
		fmt.Fprint(w, "  esac\n}\ncomplete -o default -F _sboot sboot\n")
	case "zsh":
		fmt.Fprintf(w, "#compdef sboot\n# sboot completion for zsh — add to ~/.zshrc:  source <(sboot completion zsh)\n_sboot() {\n  if (( CURRENT == 2 )); then\n    compadd -- %s; return\n  fi\n  case \"$words[2]\" in\n", cmds)
		for _, c := range commandTable {
			words := strings.Join(flagsFor(c), " ")
			if c.name == "completion" {
				words = strings.Join(completionShells, " ")
			}
			fmt.Fprintf(w, "    %s) compadd -- %s; _files ;;\n", c.name, words)
		}
		fmt.Fprint(w, "  esac\n}\nif [ \"$funcstack[1]\" = \"_sboot\" ]; then _sboot \"$@\"; else compdef _sboot sboot; fi\n")
	case "fish":
		fmt.Fprint(w, "# sboot completion for fish — run once:  sboot completion fish > ~/.config/fish/completions/sboot.fish\n")
		fmt.Fprintf(w, "complete -c sboot -n __fish_use_subcommand -f -a '%s'\n", cmds)
		for _, c := range commandTable {
			if c.name == "completion" {
				fmt.Fprintf(w, "complete -c sboot -n '__fish_seen_subcommand_from completion' -f -a '%s'\n", strings.Join(completionShells, " "))
				continue
			}
			for _, f := range flagsFor(c) {
				fmt.Fprintf(w, "complete -c sboot -n '__fish_seen_subcommand_from %s' -l %s\n", c.name, strings.TrimPrefix(f, "--"))
			}
		}
	case "powershell":
		fmt.Fprint(w, "# sboot completion for PowerShell — add to $PROFILE:  sboot completion powershell | Out-String | Invoke-Expression\n")
		fmt.Fprint(w, "Register-ArgumentCompleter -Native -CommandName sboot, sboot.exe -ScriptBlock {\n  param($wordToComplete, $commandAst, $cursorPosition)\n  $flags = @{\n")
		for _, c := range commandTable {
			words := flagsFor(c)
			if c.name == "completion" {
				words = completionShells
			}
			fmt.Fprintf(w, "    '%s' = @('%s')\n", c.name, strings.Join(words, "', '"))
		}
		fmt.Fprintf(w, "  }\n  $elems = @($commandAst.CommandElements | ForEach-Object { $_.ToString() })\n  if ($elems.Count -le 1 -or ($elems.Count -eq 2 -and $wordToComplete)) { $cands = @('%s') }\n  else { $cands = $flags[$elems[1]] }\n  $cands | Where-Object { $_ -like \"$wordToComplete*\" } | ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }\n}\n", strings.Join(commandNames(), "', '"))
	default:
		return fmt.Errorf("no completion for %q — one of: %s", shell, strings.Join(completionShells, ", "))
	}
	return nil
}
