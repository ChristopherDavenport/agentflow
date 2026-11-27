package workflow

import (
	"reflect"
	"strings"
	"testing"
)

// The skill body says how to read every key a WORKFLOW.md may hold, or
// names it as one to leave be, so a key added to the format is one the
// text has to account for.
func TestTheSkillBodyNamesEveryKey(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[frontmatter](), reflect.TypeFor[settings]()} {
		for i := range typ.NumField() {
			key, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
			if key != "" && !strings.Contains(SkillBody(), "`"+key+"`") && !strings.Contains(SkillBody(), "`"+key+":") && !strings.Contains(SkillBody(), "`"+key+".") {
				t.Errorf("%s: the skill body does not name %q", typ.Name(), key)
			}
		}
	}
	if !strings.Contains(SkillBody(), "`metadata."+MetadataKey+"`") {
		t.Errorf("the skill body does not point at metadata.%s", MetadataKey)
	}
}
