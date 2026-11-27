package workflow

import (
	_ "embed"
	"strings"

	"github.com/ChristopherDavenport/agentskill"
)

//go:embed body.md
var body string

// SkillBody is the text a workflow skill's SKILL.md opens its body
// with. A host that runs workflows never shows a model the body; one
// that does not loads the skill as any other and its model follows the
// body. This text points that model at the file metadata.agentflow
// names, and says how to follow it as agentflow would: the diagram, the
// kinds of step, outcomes, values and limits. It names no skill, so
// every workflow skill can carry it as it is; an author may add notes
// after it.
func SkillBody() string { return body }

// HasSkillBody says whether a skill's body opens with [SkillBody].
func HasSkillBody(s *agentskill.Skill) bool {
	return strings.HasPrefix(strings.TrimSpace(s.Body), strings.TrimSpace(body))
}
