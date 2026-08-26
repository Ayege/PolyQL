// Package emitter is stage 6 of the compiler pipeline: it renders a validated
// TelemetryIR tree back into idiomatic text in one target language.
//
// An emitter is the mirror of a parser, and shares its extensibility: it reads
// the target's registry definition for function names, operator spellings and
// canonical formatting, so adding a language means adding a YAML file and an
// emitter/parser pair rather than editing core code.
//
// # Reversing the flattening
//
// The IR is a flat, ordered pipeline; the DSLs nest. The resolver flattened
// sum by (job) (rate(x[5m])) into [RATE, SUM], and an emitter folds that back
// the other way, wrapping the rendered text once per stage. Each language wraps
// differently — PromQL nests calls, LogQL threads a pipeline into a range
// aggregation — which is exactly the difference the IR exists to absorb.
//
// # Honesty about loss
//
// Emitters do not judge translatability; the validator already did, and left its
// verdict on every node. An emitter reads those flags and renders what the target
// can express, collecting a note for everything it had to leave out. Notes are
// emitted as comments above the query, so the output stays parseable in the
// target language while still saying what went missing.
package emitter

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/polyql/polyql/pkg/compiler/ir"
	"github.com/polyql/polyql/pkg/registry"
)

// Emitter renders IR into one target language.
//
// Implementations must be safe for concurrent use: the federation proxy shares
// one registered emitter across requests, so an Emitter should hold no mutable
// state between calls.
type Emitter interface {
	// Emit renders a query. The registry supplies the target's definition; the
	// error is for a tree the emitter cannot render at all, not for a construct
	// the target merely cannot express, which is recorded as a note instead.
	Emit(query *ir.Query, reg *registry.Registry) (string, error)

	// DSL returns the language's canonical lowercase name, matching the name
	// its parser registers under and its registry file.
	DSL() string
}

// registryState holds the emitters registered by DSL name. It is package-level
// because registration happens from init functions in the per-DSL packages,
// which is what lets a binary choose its supported targets by which packages it
// imports.
var registryState = struct {
	sync.RWMutex
	emitters map[string]Emitter
}{emitters: make(map[string]Emitter)}

func normalizeDSL(dsl string) string { return strings.ToLower(strings.TrimSpace(dsl)) }

// Register adds an emitter under the name it reports from DSL, and is intended
// to be called from a package's init function.
//
// Register panics on a nil emitter, an empty DSL name, or a duplicate. Each is a
// build-time mistake, and failing at startup beats running with an emitter that
// is not the one the caller expects.
func Register(emitter Emitter) {
	if emitter == nil {
		panic("emitter: Register called with a nil Emitter")
	}
	dsl := normalizeDSL(emitter.DSL())
	if dsl == "" {
		panic("emitter: Register called with an Emitter reporting an empty DSL name")
	}

	registryState.Lock()
	defer registryState.Unlock()
	if existing, ok := registryState.emitters[dsl]; ok {
		panic(fmt.Sprintf("emitter: DSL %q is already registered by %T", dsl, existing))
	}
	registryState.emitters[dsl] = emitter
}

// Get returns the emitter registered for a DSL. The error names what is
// available, since the usual cause of a miss is a missing import.
func Get(dsl string) (Emitter, error) {
	name := normalizeDSL(dsl)

	registryState.RLock()
	defer registryState.RUnlock()
	if e, ok := registryState.emitters[name]; ok {
		return e, nil
	}
	if len(registryState.emitters) == 0 {
		return nil, fmt.Errorf("emitter: no emitter registered for DSL %q: none are registered at all, "+
			"which usually means no emitter package was imported", dsl)
	}
	return nil, fmt.Errorf("emitter: no emitter registered for DSL %q (registered: %s)",
		dsl, strings.Join(listLocked(), ", "))
}

// List returns the registered DSL names in sorted order.
func List() []string {
	registryState.RLock()
	defer registryState.RUnlock()
	return listLocked()
}

func listLocked() []string {
	names := make([]string, 0, len(registryState.emitters))
	for name := range registryState.emitters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Notes collects what an emitter could not render.
//
// Both target languages comment with "#" to end of line and have no block
// comment, so a note cannot be placed inline without truncating the expression
// after it. Notes are therefore gathered and written as full comment lines above
// the query, which keeps the rendered text parseable in the target language
// while still marking what was left out.
type Notes struct {
	lines []string
	seen  map[string]bool
}

// Addf records a note, ignoring exact duplicates so that one cause reported on
// several nodes does not repeat.
func (n *Notes) Addf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if n.seen == nil {
		n.seen = make(map[string]bool)
	}
	if n.seen[line] {
		return
	}
	n.seen[line] = true
	n.lines = append(n.lines, line)
}

// AddUnsupported records a node the validator marked as beyond the target,
// using the reason the validator gave.
func (n *Notes) AddUnsupported(reason string) {
	if reason == "" {
		reason = "this construct has no equivalent in the target language"
	}
	n.Addf("UNSUPPORTED: %s", reason)
}

// Len reports how many notes were recorded.
func (n *Notes) Len() int { return len(n.lines) }

// Lines returns the recorded notes.
func (n *Notes) Lines() []string { return n.lines }

// Prepend writes the notes as comment lines above the rendered query.
func (n *Notes) Prepend(query string) string {
	if len(n.lines) == 0 {
		return query
	}
	var b strings.Builder
	for _, line := range n.lines {
		b.WriteString("# " + line + "\n")
	}
	b.WriteString(query)
	return b.String()
}

// Unsupported reports whether a node was flagged as inexpressible, and gives the
// validator's reason.
func Unsupported(node ir.Node) (string, bool) {
	flag, reason := node.Base().Translatability()
	return reason, flag == ir.TranslatabilityUnsupported
}

// ConjunctiveMatchers flattens a span set's boolean filter into the flat matcher
// list a DSL with conjunctive selectors can write between its braces.
//
// PromQL and LogQL both put an implicit "and" between the matchers in a
// selector, so an AND-tree lowers exactly. An OR or a NOT does not: there is no
// way to write "either of these labels" inside one selector, and dropping the
// operand that did not fit would silently widen the query. Such a subtree is
// therefore left out and reported through the returned flag, which the caller
// turns into a note.
//
// The boolean returned is true when everything lowered faithfully.
func ConjunctiveMatchers(predicate ir.Predicate) ([]*ir.LabelMatcher, bool) {
	switch node := predicate.(type) {
	case nil:
		return nil, true

	case *ir.MatchPredicate:
		if node.Matcher == nil {
			return nil, true
		}
		return []*ir.LabelMatcher{node.Matcher}, true

	case *ir.LogicalPredicate:
		if node.Op == ir.LogicalOr {
			// One shape of disjunction does survive: several alternatives for
			// the same attribute are set membership, which every target here
			// writes as an anchored regex alternation.
			if matcher, ok := ir.FoldSameKeyDisjunction(node); ok {
				return []*ir.LabelMatcher{matcher}, true
			}
		}
		if node.Op != ir.LogicalAnd {
			// Anything else has no conjunctive form at all, so nothing from
			// this subtree may be kept: half a disjunction is a different query.
			return nil, false
		}
		var matchers []*ir.LabelMatcher
		faithful := true
		for _, operand := range node.Operands {
			lowered, ok := ConjunctiveMatchers(operand)
			if !ok {
				faithful = false
				continue
			}
			matchers = append(matchers, lowered...)
		}
		return matchers, faithful

	default:
		return nil, false
	}
}

// SelectorSpellable partitions matchers into those the target can write inside a
// selector and those it cannot.
//
// An operator existing in a DSL is not the same as its being writable in a
// selector. LogQL has ">", but only as a label filter after a parser stage; a
// stream selector admits =, !=, =~ and !~ and nothing else. A span query
// comparing a duration therefore lowers to a matcher that would not parse, and
// writing it anyway would produce a translation that fails on paste — the one
// outcome the round-trip suite exists to prevent.
func SelectorSpellable(def *registry.DSLDefinition, matchers []*ir.LabelMatcher) (
	spellable, unspellable []*ir.LabelMatcher) {

	for _, matcher := range matchers {
		op := matcher.Op
		// The set and containment operators are written as a regex instead, so
		// what has to be spellable is the operator they lower to.
		switch {
		case op == ir.MatchIn || op == ir.MatchContains:
			op = ir.MatchRegex
		case op == ir.MatchNotIn || op == ir.MatchNotContains:
			op = ir.MatchNotRegex
		}
		if def.SupportsIROpInContext(op, registry.OperatorContextSelector) {
			spellable = append(spellable, matcher)
			continue
		}
		unspellable = append(unspellable, matcher)
	}
	return spellable, unspellable
}

// durationUnits are the time units both target languages share, longest first.
var durationUnits = []struct {
	name string
	size time.Duration
}{
	{"y", 365 * 24 * time.Hour},
	{"w", 7 * 24 * time.Hour},
	{"d", 24 * time.Hour},
	{"h", time.Hour},
	{"m", time.Minute},
	{"s", time.Second},
	{"ms", time.Millisecond},
}

// fractionalDuration matches a duration component written with a decimal point,
// such as "1.5h". LogQL accepts those; PromQL does not.
var fractionalDuration = regexp.MustCompile(`[0-9]*\.[0-9]`)

// DurationSourceText returns the interval's original spelling when the target
// can read it, and reports whether it may be used.
//
// A duration that reached the IR as text goes back out exactly as written, so a
// LogQL "[90m]" stays "90m" instead of being re-derived as the equal but
// unfamiliar "1h30m". The one case where the original cannot be reused is a
// fractional component: LogQL writes "1.5h" and PromQL has no such form, so a
// target whose durations decompose into whole units gets the canonical
// rendering instead.
func DurationSourceText(interval ir.Interval, format registry.DurationFormat) (string, bool) {
	if interval.SourceText == "" {
		return "", false
	}
	if format == registry.DurationLargestUnit && fractionalDuration.MatchString(interval.SourceText) {
		return "", false
	}
	return interval.SourceText, true
}

// FormatDuration renders an interval in the target's canonical duration form,
// preferring the spelling the query was written with when the target can read
// it. See DurationSourceText for when it cannot.
func FormatDuration(interval ir.Interval, format registry.DurationFormat) string {
	if text, ok := DurationSourceText(interval, format); ok {
		return text
	}
	d := interval.Duration()
	if d == 0 {
		return "0s"
	}
	var b strings.Builder
	if d < 0 {
		b.WriteByte('-')
		d = -d
	}
	for _, u := range durationUnits {
		if d >= u.size {
			n := d / u.size
			fmt.Fprintf(&b, "%d%s", n, u.name)
			d -= n * u.size
		}
	}
	if b.Len() == 0 {
		// Shorter than a millisecond, which neither target can write.
		return "0s"
	}
	return b.String()
}

// QuoteString renders a string literal in the target's canonical quoting.
func QuoteString(s string, quoting registry.StringQuoting) string {
	switch quoting {
	case registry.StringQuoteBacktick:
		// A backquoted string cannot contain a backquote, so fall back to
		// double quotes when the value holds one.
		if !strings.ContainsRune(s, '`') {
			return "`" + s + "`"
		}
	case registry.StringQuoteSingle:
		if !strings.ContainsAny(s, "'\\") {
			return "'" + s + "'"
		}
	}
	return strconv.Quote(s)
}

// FormatNumber renders a numeric literal so the target re-parses it to the same
// value.
func FormatNumber(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// LiteralText renders an IR literal's value as source text.
func LiteralText(literal *ir.LiteralExpr, quoting registry.StringQuoting) string {
	if literal == nil || literal.Value == nil {
		return "NULL"
	}
	switch value := literal.Value.(type) {
	case string:
		if literal.Type == ir.DataTypeString || literal.Type == ir.DataTypeBinaryString {
			return QuoteString(value, quoting)
		}
		// A duration or byte size that reached the IR as text goes back out
		// exactly as written, which is what keeps "20MB" from becoming a raw
		// count of bytes.
		return value
	case float64:
		return FormatNumber(value)
	case int:
		return strconv.Itoa(value)
	case bool:
		return strconv.FormatBool(value)
	case ir.Interval:
		return FormatDuration(value, registry.DurationLargestUnit)
	default:
		return fmt.Sprintf("%v", value)
	}
}
