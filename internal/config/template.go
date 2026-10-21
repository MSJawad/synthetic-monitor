package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Header values and request bodies are templates: ${NAME} is replaced by a
// variable's value when a request is made, and $$ stands for a literal $.
// Any other $ is literal too, so ordinary text needs no escaping. Variables
// are environment variables, which keeps secrets out of the checks file,
// and, in a multi-step check, values extracted from earlier responses.

// Env looks up an environment variable, like os.LookupEnv.
type Env func(name string) (string, bool)

// varName is the syntax of a variable name, the same as a shell's.
var varName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// scanTemplate splits s into literal text and variable references, calling
// lit and ref for each in order.
func scanTemplate(s string, lit func(string), ref func(string)) error {
	for {
		i := strings.IndexByte(s, '$')
		if i < 0 || i == len(s)-1 {
			lit(s)
			return nil
		}
		lit(s[:i])
		switch s[i+1] {
		case '$':
			lit("$")
			s = s[i+2:]
		case '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return fmt.Errorf("unclosed ${ in %q", truncate(s[i:]))
			}
			name := s[i+2 : i+2+end]
			if !varName.MatchString(name) {
				return fmt.Errorf("${%s} is not a valid variable name", name)
			}
			ref(name)
			s = s[i+3+end:]
		default:
			lit("$")
			s = s[i+1:]
		}
	}
}

// templateRefs returns the variables template s refers to.
func templateRefs(s string) ([]string, error) {
	var names []string
	err := scanTemplate(s, func(string) {}, func(name string) { names = append(names, name) })
	return names, err
}

// Expand returns template s with each variable replaced by its value from
// lookup. s must have passed validation.
func Expand(s string, lookup func(name string) string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	var b strings.Builder
	_ = scanTemplate(s, func(text string) { b.WriteString(text) }, func(name string) { b.WriteString(lookup(name)) })
	return b.String()
}

// truncate shortens s for an error message.
func truncate(s string) string {
	if len(s) > 20 {
		return s[:20] + "..."
	}
	return s
}
