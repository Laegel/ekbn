package serve

import "strings"

// BuildArgv resolves a CommandSpec into the program and full argument list
// to exec: {workdir} is substituted into Program and every Args element,
// and prompt is appended as the final argument. Substitution happens
// unconditionally, using whatever workdir the caller passes ("" where
// there's no meaningful one) — a no-op if the spec contains no {workdir}
// token, which is the common case outside the agent-attempt path.
func BuildArgv(cmd CommandSpec, workdir, prompt string) (program string, args []string) {
	program = strings.ReplaceAll(cmd.Program, "{workdir}", workdir)
	args = make([]string, 0, len(cmd.Args)+1)
	for _, a := range cmd.Args {
		args = append(args, strings.ReplaceAll(a, "{workdir}", workdir))
	}
	return program, append(args, prompt)
}
