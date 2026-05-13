package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Decision is the engine's conclusion about a tool call.
type Decision int

const (
	DecisionAllow   Decision = iota
	DecisionApprove          // suspend for human approval
	DecisionBlock
)

func (d Decision) String() string {
	switch d {
	case DecisionAllow:
		return "allow"
	case DecisionApprove:
		return "approve"
	case DecisionBlock:
		return "block"
	}
	return "unknown"
}

// Thresholds configures the risk-score cutoffs.
//
//	score < ApproveThreshold     → allow
//	score < BlockThreshold       → require human approval
//	score >= BlockThreshold      → block
//
// Defaults: ApproveThreshold=30, BlockThreshold=80.
type Thresholds struct {
	ApproveThreshold int
	BlockThreshold   int
}

// DefaultThresholds returns the engine's default cutoffs.
func DefaultThresholds() Thresholds {
	return Thresholds{ApproveThreshold: 30, BlockThreshold: 80}
}

// SeverityWeight is how much each severity contributes to the total score.
// Multiple findings sum; the total is capped at 100.
func SeverityWeight(s Severity) int {
	switch s {
	case SevCritical:
		return 90
	case SevHigh:
		return 50
	case SevMedium:
		return 25
	case SevLow:
		return 10
	}
	return 0
}

// Engine evaluates tool calls against policy. Slice 4: risk-score based,
// three-way decision (allow / approve / block).
type Engine struct {
	rules      *Registry
	denyPaths  []denyPathRule
	Thresholds Thresholds
}

// NewEngine constructs an engine with the default rule registry, the
// caller's path denylist, and the default risk-score thresholds.
func NewEngine(denyPaths []string) *Engine {
	e := &Engine{
		rules:      DefaultRegistry(),
		Thresholds: DefaultThresholds(),
	}
	for _, p := range denyPaths {
		e.denyPaths = append(e.denyPaths, compileDenyPath(p))
	}
	return e
}

// WithThresholds sets non-default thresholds and returns the engine for
// chaining.
func (e *Engine) WithThresholds(t Thresholds) *Engine {
	e.Thresholds = t
	return e
}

// Result is what the engine returns for a single tool call.
type Result struct {
	Decision  Decision
	RiskScore int       // 0..100, sum of finding weights
	Findings  []Finding
	// BlockReason / ApproveReason is the most-severe finding that drove
	// the decision, for user-facing error messages / approval prompts.
	BlockReason   *Finding
	ApproveReason *Finding
}

// Evaluate inspects a tools/call request and returns the policy result.
// method is the JSON-RPC method (typically "tools/call"); toolName is the
// inner tool name; params is the raw params payload.
func (e *Engine) Evaluate(method, toolName string, params json.RawMessage) Result {
	var findings []Finding

	// Walk the params tree, evaluating every string against the rules and
	// against the path denylist.
	walkStrings(params, "params", func(jsonPath, value string) {
		findings = append(findings, e.rules.Eval(value, jsonPath)...)
		for _, dp := range e.denyPaths {
			if dp.matches(value) {
				findings = append(findings, Finding{
					Category:    CatSensitivePath,
					Rule:        "user-deny-path",
					Description: fmt.Sprintf("Path %q is on the user-configured deny list (pattern %q)", truncate(value, 80), dp.original),
					Severity:    SevHigh,
					JSONPath:    jsonPath,
					Value:       truncate(value, MaxValueLen),
				})
			}
		}
	})

	// Also evaluate the tool name itself (a malicious server could expose a
	// tool literally named "rm -rf /") and the method name.
	findings = append(findings, e.rules.Eval(toolName, "params.name")...)

	// Compute risk score from finding weights (capped at 100).
	score := 0
	var topFinding *Finding
	for i := range findings {
		score += SeverityWeight(findings[i].Severity)
		if topFinding == nil || findings[i].Severity > topFinding.Severity {
			topFinding = &findings[i]
		}
	}
	if score > 100 {
		score = 100
	}

	res := Result{Findings: findings, RiskScore: score, Decision: DecisionAllow}
	switch {
	case score >= e.Thresholds.BlockThreshold:
		res.Decision = DecisionBlock
		res.BlockReason = topFinding
	case score >= e.Thresholds.ApproveThreshold:
		res.Decision = DecisionApprove
		res.ApproveReason = topFinding
	}
	return res
}

// ---------------------------------------------------------------------------
// Path deny rules
// ---------------------------------------------------------------------------

type denyPathRule struct {
	original string
	pattern  string // normalized pattern for matching
	isGlob   bool
}

func compileDenyPath(p string) denyPathRule {
	r := denyPathRule{original: p}
	expanded := expandHome(p)
	r.pattern = strings.ToLower(filepath.ToSlash(expanded))
	if strings.ContainsAny(p, "*?[") {
		r.isGlob = true
	}
	return r
}

// expandHome replaces a leading "~/" or "~\" with the user's home directory.
// Any other use of "~" is left alone (paths like ~foo are not POSIX home
// expansion shortcuts — they would mean another user's home, which we do
// not support here).
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, "~\\") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	return filepath.Join(home, p[2:])
}

// matches reports whether a candidate path argument is denied by this rule.
// Comparison is case-insensitive on Windows-style paths. Substring matching
// is used for plain rules ("contains ~/.ssh"); glob matching is used when
// the pattern contains wildcards.
func (r denyPathRule) matches(value string) bool {
	v := strings.ToLower(filepath.ToSlash(value))
	if r.isGlob {
		if ok, _ := filepath.Match(r.pattern, v); ok {
			return true
		}
		// Also try matching just the base name and any suffix segment.
		if ok, _ := filepath.Match(r.pattern, filepath.Base(v)); ok {
			return true
		}
		return false
	}
	return strings.Contains(v, r.pattern)
}

// ---------------------------------------------------------------------------
// JSON walker
// ---------------------------------------------------------------------------

// walkStrings invokes fn for every string value reachable in raw, building
// a dotted JSONPath as it goes. It is intentionally permissive: nil raw,
// invalid JSON, and unexpected shapes are skipped silently.
func walkStrings(raw json.RawMessage, prefix string, fn func(path, value string)) {
	if len(raw) == 0 {
		return
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return
	}
	walkAny(v, prefix, fn)
}

func walkAny(v any, prefix string, fn func(path, value string)) {
	switch t := v.(type) {
	case string:
		fn(prefix, t)
	case []any:
		for i, e := range t {
			walkAny(e, fmt.Sprintf("%s[%d]", prefix, i), fn)
		}
	case map[string]any:
		for k, e := range t {
			walkAny(e, prefix+"."+k, fn)
		}
	}
}
