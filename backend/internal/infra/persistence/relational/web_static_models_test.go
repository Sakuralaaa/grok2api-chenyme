package relational

import (
	"sort"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
)

func TestWebBasicStaticModelsMatchCatalog(t *testing.T) {
	var want []string
	for _, spec := range webprovider.Catalog() {
		if spec.MinimumTier == account.WebTierBasic {
			want = append(want, spec.UpstreamModel)
		}
	}
	got := append([]string(nil), webBasicStaticModels...)
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("webBasicStaticModels = %v, catalog Basic models = %v", got, want)
	}
	quoted := make([]string, 0, len(webBasicStaticModels))
	for _, value := range webBasicStaticModels {
		quoted = append(quoted, "'"+value+"'")
	}
	if strings.Join(quoted, ", ") != webBasicStaticModelsSQL {
		t.Fatalf("webBasicStaticModelsSQL out of sync: %s", webBasicStaticModelsSQL)
	}
}
