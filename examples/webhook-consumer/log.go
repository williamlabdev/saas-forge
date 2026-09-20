package main

import (
	"log"
	"strings"
)

// logf is this consumer's only log sink, and it exists for one reason: every
// value worth logging here — event type, entry key, delivery id, a parse error
// echoing the payload — arrives from the webhook body or its headers.
//
// The signature check in ServeHTTP runs BEFORE any of this and is not the same
// guarantee. It proves the sender holds the shared secret; it says nothing
// about what a tenant's editor typed into an entry title. A title containing a
// newline, logged raw, forges a second log line that never happened — and log
// lines are what an operator reads when deciding whether a takedown landed.
//
// So string and error arguments get their line breaks escaped before fmt sees
// them. Escaping rather than stripping: "a\nb" logs as `a\nb`, which stays
// legible and stays one line. Numbers and the rest pass through untouched —
// they cannot carry a line break.
func logf(format string, args ...any) {
	// args is a fresh slice per call (variadic), so rewriting it in place
	// cannot surprise the caller.
	for i, a := range args {
		switch v := a.(type) {
		case string:
			args[i] = escapeLineBreaks(v)
		case error:
			args[i] = escapeLineBreaks(v.Error())
		}
	}
	// gosec's G706 taint analysis recognises no sanitiser — not %q, not a
	// replacer, not this function. It tracks payload-derived values to the sink
	// and stops. The escaping above is the actual fix; this annotation only
	// says the linter cannot see it, and it is deliberately the ONE place in
	// this package that says so.
	log.Printf(format, args...) //#nosec G706 -- every string and error argument is escaped directly above
}

var lineBreakEscaper = strings.NewReplacer("\n", `\n`, "\r", `\r`)

func escapeLineBreaks(s string) string { return lineBreakEscaper.Replace(s) }
