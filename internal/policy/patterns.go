// Package policy contains the Sentinel detection engine.
//
// Slice 3 scope: regex- and heuristic-based detection of common attack
// patterns in MCP tool call arguments. No ML, no third-party detectors —
// all rules are code in this package. Future slices add risk scoring,
// approval gating, and ML detection trained on telemetry we collect first.
package policy

import (
	"regexp"
	"strings"
	"unicode"
)

// Severity describes how dangerous a finding is.
type Severity int

const (
	SevInfo Severity = iota
	SevLow
	SevMedium
	SevHigh
	SevCritical
)

func (s Severity) String() string {
	switch s {
	case SevInfo:
		return "info"
	case SevLow:
		return "low"
	case SevMedium:
		return "medium"
	case SevHigh:
		return "high"
	case SevCritical:
		return "critical"
	}
	return "unknown"
}

// Category groups patterns by attack type.
type Category string

const (
	CatSensitivePath    Category = "sensitive-path"
	CatPathTraversal    Category = "path-traversal"
	CatCommandInjection Category = "command-injection"
	CatUnicodeSmuggling Category = "unicode-smuggling"
	CatPromptInjection  Category = "prompt-injection"
	CatSecretLike       Category = "secret-like"
)

// Finding is one detection result.
type Finding struct {
	Category    Category
	Rule        string   // short id, e.g. "ssh-private-key-path"
	Description string   // human-readable explanation
	Severity    Severity
	JSONPath    string   // path within the argument tree, e.g. "params.arguments.path"
	Value       string   // the matched value, truncated to MaxValueLen
}

// MaxValueLen caps the length of Finding.Value to keep audit rows compact.
const MaxValueLen = 256

// Rule is one compiled detection rule.
type Rule struct {
	Category    Category
	ID          string
	Description string
	Severity    Severity
	// Match returns true if the value triggers the rule. value is already
	// lowercased for case-insensitive matching where appropriate.
	Match func(value string) bool
}

// Registry is the in-process set of active rules.
type Registry struct {
	rules []Rule
}

// DefaultRegistry returns the standard built-in rule set.
func DefaultRegistry() *Registry {
	r := &Registry{}
	r.rules = append(r.rules, sensitivePathRules()...)
	r.rules = append(r.rules, pathTraversalRules()...)
	r.rules = append(r.rules, commandInjectionRules()...)
	r.rules = append(r.rules, unicodeSmugglingRules()...)
	r.rules = append(r.rules, promptInjectionRules()...)
	r.rules = append(r.rules, secretLikeRules()...)
	return r
}

// Rules returns a copy of the active rule set for inspection.
func (r *Registry) Rules() []Rule {
	out := make([]Rule, len(r.rules))
	copy(out, r.rules)
	return out
}

// Eval runs every rule against value and returns the findings.
// jsonPath is recorded on each finding for audit context.
func (r *Registry) Eval(value, jsonPath string) []Finding {
	if value == "" {
		return nil
	}
	var out []Finding
	for _, rule := range r.rules {
		if rule.Match(value) {
			out = append(out, Finding{
				Category:    rule.Category,
				Rule:        rule.ID,
				Description: rule.Description,
				Severity:    rule.Severity,
				JSONPath:    jsonPath,
				Value:       truncate(value, MaxValueLen),
			})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Sensitive paths
// ---------------------------------------------------------------------------

// sensitivePathRules denies paths that almost never appear in legitimate AI
// tool calls. Match is case-insensitive on Windows-ish path separators.
func sensitivePathRules() []Rule {
	// Substring matches against the normalized lowercased path. We
	// intentionally err toward false positives — the user can carve out
	// specific allowed paths via env.allow_paths in slice 3+.
	patterns := []struct {
		id, desc string
		needles  []string
	}{
		{
			id:   "ssh-secrets",
			desc: "Attempt to access SSH key material",
			needles: []string{
				"/.ssh/", "\\.ssh\\",
				"id_rsa", "id_ed25519", "id_ecdsa", "id_dsa",
				"authorized_keys",
				"known_hosts",
			},
		},
		{
			id:   "aws-credentials",
			desc: "Attempt to access AWS credentials",
			needles: []string{
				"/.aws/credentials", "/.aws/config",
				"\\.aws\\credentials", "\\.aws\\config",
			},
		},
		{
			id:   "cloud-credentials",
			desc: "Attempt to access cloud provider credentials",
			needles: []string{
				"/.config/gcloud", "\\.config\\gcloud",
				"/.azure/", "\\.azure\\",
				"/.kube/config", "\\.kube\\config",
			},
		},
		{
			id:   "system-secrets",
			desc: "Attempt to read OS-level credential stores",
			needles: []string{
				"/etc/passwd",
				"/etc/shadow",
				"/etc/sudoers",
				"/etc/ssh/",
				"\\windows\\system32\\config\\sam",
				"\\windows\\system32\\config\\security",
			},
		},
		{
			id:   "package-credentials",
			desc: "Attempt to access package-manager credentials",
			needles: []string{
				"/.npmrc", "\\.npmrc",
				"/.pypirc", "\\.pypirc",
				"/.netrc", "\\.netrc",
				"/.docker/config.json", "\\.docker\\config.json",
			},
		},
		{
			id:   "shell-history",
			desc: "Attempt to read shell history (often contains pasted secrets)",
			needles: []string{
				"/.bash_history", "\\.bash_history",
				"/.zsh_history", "\\.zsh_history",
				"/.psql_history", "\\.psql_history",
				"/.mysql_history", "\\.mysql_history",
				"/consolehost_history.txt", "\\consolehost_history.txt",
			},
		},
		{
			id:   "browser-storage",
			desc: "Attempt to access browser session/credential storage",
			needles: []string{
				"/login data", "\\login data",
				"/cookies.sqlite", "\\cookies.sqlite",
				"/key3.db", "\\key3.db",
				"/key4.db", "\\key4.db",
				"/places.sqlite", "\\places.sqlite",
			},
		},
		{
			id:   "git-credentials",
			desc: "Attempt to access git credential storage",
			needles: []string{
				"/.git-credentials", "\\.git-credentials",
				"/.config/git/credentials",
			},
		},
	}

	var rules []Rule
	for _, p := range patterns {
		needles := p.needles
		p := p
		rules = append(rules, Rule{
			Category:    CatSensitivePath,
			ID:          p.id,
			Description: p.desc,
			Severity:    SevHigh,
			Match: func(v string) bool {
				low := strings.ToLower(v)
				for _, n := range needles {
					if strings.Contains(low, n) {
						return true
					}
				}
				return false
			},
		})
	}
	return rules
}

// ---------------------------------------------------------------------------
// Path traversal
// ---------------------------------------------------------------------------

var (
	rePathTraversalDotSlash    = regexp.MustCompile(`(?:^|[/\\])\.\.(?:[/\\]|$)`)
	rePathTraversalEncoded     = regexp.MustCompile(`(?i)%2e%2e|%252e%252e|\.\.%2f|\.\.%5c`)
	rePathTraversalDoubleDot   = regexp.MustCompile(`\.\.\.+`) // ".../" exploits in some parsers
)

func pathTraversalRules() []Rule {
	return []Rule{
		{
			Category:    CatPathTraversal,
			ID:          "dot-slash",
			Description: `Path traversal sequence (../, ..\)`,
			Severity:    SevHigh,
			Match:       func(v string) bool { return rePathTraversalDotSlash.MatchString(v) },
		},
		{
			Category:    CatPathTraversal,
			ID:          "encoded",
			Description: "URL-encoded path traversal (%2e%2e and variants)",
			Severity:    SevHigh,
			Match:       func(v string) bool { return rePathTraversalEncoded.MatchString(v) },
		},
		{
			Category:    CatPathTraversal,
			ID:          "triple-dot",
			Description: "Suspicious triple-or-more dot sequence",
			Severity:    SevMedium,
			Match:       func(v string) bool { return rePathTraversalDoubleDot.MatchString(v) },
		},
	}
}

// ---------------------------------------------------------------------------
// Command injection
// ---------------------------------------------------------------------------

var (
	reShellMetacharsHigh = regexp.MustCompile("[`$](?:\\(|\\{)|\\$\\(|;\\s*\\w+|&&\\s*\\w+|\\|\\|\\s*\\w+|>\\s*/dev/|<\\s*/dev/")
	reDangerousCmds      = regexp.MustCompile(`(?i)\b(?:rm\s+-rf|dd\s+if=|mkfs(?:\.\w+)?\s|:\(\)\s*\{|chmod\s+777|chown\s+-R|curl\s+[^|]*\|\s*(?:sh|bash)|wget\s+[^|]*\|\s*(?:sh|bash))`)
)

func commandInjectionRules() []Rule {
	return []Rule{
		{
			Category:    CatCommandInjection,
			ID:          "shell-metachars",
			Description: "Shell metacharacters suggesting command injection",
			Severity:    SevHigh,
			Match:       func(v string) bool { return reShellMetacharsHigh.MatchString(v) },
		},
		{
			Category:    CatCommandInjection,
			ID:          "dangerous-command",
			Description: "Known-dangerous shell command pattern",
			Severity:    SevCritical,
			Match:       func(v string) bool { return reDangerousCmds.MatchString(v) },
		},
	}
}

// ---------------------------------------------------------------------------
// Unicode smuggling
// ---------------------------------------------------------------------------

func unicodeSmugglingRules() []Rule {
	return []Rule{
		{
			Category:    CatUnicodeSmuggling,
			ID:          "tag-chars",
			Description: "Unicode tag characters (U+E0000-U+E007F) used to smuggle hidden instructions",
			Severity:    SevCritical,
			Match: func(v string) bool {
				for _, r := range v {
					if r >= 0xE0000 && r <= 0xE007F {
						return true
					}
				}
				return false
			},
		},
		{
			Category:    CatUnicodeSmuggling,
			ID:          "rtl-override",
			Description: "Right-to-left override or bidi control characters",
			Severity:    SevHigh,
			Match: func(v string) bool {
				for _, r := range v {
					switch r {
					case 0x202A, 0x202B, 0x202C, 0x202D, 0x202E,
						0x2066, 0x2067, 0x2068, 0x2069:
						return true
					}
				}
				return false
			},
		},
		{
			Category:    CatUnicodeSmuggling,
			ID:          "zero-width",
			Description: "Zero-width characters used to hide content",
			Severity:    SevMedium,
			Match: func(v string) bool {
				count := 0
				for _, r := range v {
					switch r {
					case 0x200B, 0x200C, 0x200D, 0x2060, 0xFEFF:
						count++
						if count >= 3 {
							return true
						}
					}
				}
				return false
			},
		},
		{
			Category:    CatUnicodeSmuggling,
			ID:          "unusual-format-chars",
			Description: "High concentration of Unicode format (Cf) characters",
			Severity:    SevLow,
			Match: func(v string) bool {
				if len(v) < 16 {
					return false
				}
				cfCount, total := 0, 0
				for _, r := range v {
					total++
					if unicode.Is(unicode.Cf, r) {
						cfCount++
					}
				}
				return total > 0 && float64(cfCount)/float64(total) > 0.1
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Prompt injection (basic regex / heuristic only; ML in v0.3+)
// ---------------------------------------------------------------------------

var rePromptInjection = regexp.MustCompile(`(?i)` +
	`ignore\s+(?:all\s+)?previous\s+(?:instructions|prompts|directives)|` +
	`disregard\s+(?:all\s+)?(?:previous|above|system)\s+(?:instructions|prompts)|` +
	`forget\s+(?:everything|all\s+(?:above|previous|prior))|` +
	`new\s+(?:instructions|system\s+prompt)\s*:|` +
	`you\s+are\s+now\s+(?:a\s+)?[a-z]+\s+(?:assistant|agent|bot)\s+without\s+restrictions|` +
	`reveal\s+(?:your\s+)?(?:system\s+prompt|instructions|hidden\s+prompt)|` +
	`print\s+(?:your\s+)?(?:system\s+prompt|hidden\s+instructions)|` +
	`developer\s+mode\s+(?:enabled|activated|on)|` +
	`jailbreak\s+mode|DAN\s+mode`)

func promptInjectionRules() []Rule {
	return []Rule{
		{
			Category:    CatPromptInjection,
			ID:          "instruction-override",
			Description: "Phrase commonly used to override system instructions",
			Severity:    SevHigh,
			Match:       func(v string) bool { return rePromptInjection.MatchString(v) },
		},
	}
}

// ---------------------------------------------------------------------------
// Secret-like strings (high entropy, recognizable shapes)
// ---------------------------------------------------------------------------

var (
	reAWSAccessKey    = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	reGitHubToken     = regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b`)
	reSlackToken      = regexp.MustCompile(`\bxox[abpsr]-[A-Za-z0-9-]{10,}\b`)
	reGenericJWT      = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
	reOpenAIKey       = regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`)
	reAnthropicKey    = regexp.MustCompile(`\bsk-ant-(?:api|sid)\d+-[A-Za-z0-9_-]{20,}\b`)
)

func secretLikeRules() []Rule {
	return []Rule{
		{
			Category:    CatSecretLike,
			ID:          "aws-access-key",
			Description: "Looks like an AWS access key id",
			Severity:    SevCritical,
			Match:       func(v string) bool { return reAWSAccessKey.MatchString(v) },
		},
		{
			Category:    CatSecretLike,
			ID:          "github-token",
			Description: "Looks like a GitHub personal access token",
			Severity:    SevCritical,
			Match:       func(v string) bool { return reGitHubToken.MatchString(v) },
		},
		{
			Category:    CatSecretLike,
			ID:          "slack-token",
			Description: "Looks like a Slack token",
			Severity:    SevHigh,
			Match:       func(v string) bool { return reSlackToken.MatchString(v) },
		},
		{
			Category:    CatSecretLike,
			ID:          "jwt",
			Description: "Looks like a JSON Web Token",
			Severity:    SevMedium,
			Match:       func(v string) bool { return reGenericJWT.MatchString(v) },
		},
		{
			Category:    CatSecretLike,
			ID:          "openai-key",
			Description: "Looks like an OpenAI API key",
			Severity:    SevCritical,
			Match:       func(v string) bool { return reOpenAIKey.MatchString(v) },
		},
		{
			Category:    CatSecretLike,
			ID:          "anthropic-key",
			Description: "Looks like an Anthropic API key",
			Severity:    SevCritical,
			Match:       func(v string) bool { return reAnthropicKey.MatchString(v) },
		},
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
