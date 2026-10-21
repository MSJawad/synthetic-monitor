package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"

	"github.com/MSJawad/synthetic-monitor/internal/config"
)

// stepsProber runs a multi-step check: requests in order, each able to use
// variables extracted from earlier responses and the cookies they set.
type stepsProber struct {
	steps     []step
	env       func(name string) string
	transport http.RoundTripper
}

type step struct {
	name    string
	req     request
	extract map[string][]string // variable name to its path in the JSON body
}

func newSteps(steps []config.Step, opts Options) *stepsProber {
	p := &stepsProber{env: opts.lookupEnv(), transport: newHTTPClient(opts).Transport}
	for _, s := range steps {
		st := step{name: s.Name, req: newRequest(&s.HTTP), extract: make(map[string][]string)}
		for name, path := range s.Extract {
			st.extract[name] = strings.Split(path, ".")
		}
		p.steps = append(p.steps, st)
	}
	return p
}

func (p *stepsProber) Probe(ctx context.Context) Result {
	// A fresh jar per run, like a fresh connection per probe: every run
	// starts as a new visitor, so a session left by the last run cannot
	// hide a broken sign-in.
	jar, err := cookiejar.New(nil)
	if err != nil {
		return Result{Reason: ReasonOther, Err: err}
	}
	client := &http.Client{Transport: p.transport, Jar: jar}
	vars := make(map[string]string)
	lookup := func(name string) string {
		if v, ok := vars[name]; ok {
			return v
		}
		return p.env(name)
	}
	for i, s := range p.steps {
		r, body := s.req.do(ctx, client, lookup)
		if !r.OK() {
			return Result{Reason: r.Reason, Err: fmt.Errorf("step %d %q: %w", i+1, s.name, r.Err)}
		}
		for name, path := range s.extract {
			v, err := extractJSON(body, path)
			if err != nil {
				return Result{Reason: ReasonHTTPBody, Err: fmt.Errorf("step %d %q: extracting %s: %w", i+1, s.name, name, err)}
			}
			vars[name] = v
		}
	}
	return Result{}
}

// extractJSON returns the string, number or boolean at path in a JSON
// document. Errors never quote values from the body, which may be secrets.
func extractJSON(body []byte, path []string) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // keeps large IDs exact rather than rounding them to float64
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", errors.New("the response is not JSON")
	}
	for i, key := range path {
		where := strings.Join(path[:i+1], ".")
		switch node := v.(type) {
		case map[string]any:
			next, ok := node[key]
			if !ok {
				return "", fmt.Errorf("%s is not in the response", where)
			}
			v = next
		case []any:
			n, err := strconv.Atoi(key)
			if err != nil || n < 0 || n >= len(node) {
				return "", fmt.Errorf("%s is not in the response", where)
			}
			v = node[n]
		default:
			return "", fmt.Errorf("%s is not in the response", where)
		}
	}
	switch v := v.(type) {
	case string:
		return v, nil
	case json.Number:
		return v.String(), nil
	case bool:
		return strconv.FormatBool(v), nil
	}
	return "", fmt.Errorf("%s is not a string, number or boolean", strings.Join(path, "."))
}
