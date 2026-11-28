package identify

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/ALi3naTEd0/couchlog/internal/store"
)

// ProviderCatalog is a catalog that knows where titles stream (*TMDB).
type ProviderCatalog interface {
	Providers(ctx context.Context, k Kind, id int) (map[string][]int, error)
}

// providerIDs are TMDB's watch-provider ids for the apps' services; one
// app may count as several (HBO Max was Max for a while).
var providerIDs = []struct {
	match string
	ids   []int
}{
	{"netflix", []int{8, 1796}},
	{"disney", []int{337}},
	{"wbd", []int{1899, 384}}, // HBO Max (com.wbd.stream), Max
	{"hbo", []int{1899, 384}},
	{"amazon", []int{119, 9}},
	{"primevideo", []int{119, 9}},
	{"appletv", []int{350, 2}},
	{"paramount", []int{531, 582}},
	{"crunchyroll", []int{283}},
	{"mubi", []int{11}},
	{"vix", []int{457}},
	{"pluto", []int{300}},
	{"tubi", []int{73}},
	{"peacock", []int{386, 387}},
	{"hulu", []int{15}},
	{"starz", []int{43}},
}

// ProvidersOf returns the TMDB provider ids for an app, or nil.
func ProvidersOf(appID string) []int {
	a := strings.ToLower(appID)
	for _, p := range providerIDs {
		if strings.Contains(a, p.match) {
			return p.ids
		}
	}
	return nil
}

// onService finds the one title whose name shares the published title's
// distinctive words and that streams on the app's own service in your
// country. Apps publish localized names TMDB does not list (Disney+ says
// "Looksmaxxing extremo: Asesinato en Pattaya" for Get Huge: Looksmaxxing
// to Death); the words they keep, narrowed to what that service carries
// there, leave one candidate or none.
func (id *Identifier) onService(ctx context.Context, appID, title string) (*Title, error) {
	pc, ok := id.Catalog.(ProviderCatalog)
	ids := ProvidersOf(appID)
	if !ok || len(ids) == 0 || id.Region == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var cands []Title
	for _, q := range searchTerms(title) {
		found, err := id.Catalog.Search(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, t := range found {
			key := fmt.Sprintf("%s:%d", t.Kind, t.TMDB)
			if seen[key] || !sharesTerm(t, q) {
				continue
			}
			seen[key] = true
			cands = append(cands, t)
		}
	}
	if len(cands) > 20 {
		cands = cands[:20] // a word that common narrows nothing; keep the calls bounded
	}
	var hits []Title
	for _, t := range cands {
		prov, err := pc.Providers(ctx, t.Kind, t.TMDB)
		if err != nil {
			continue
		}
		if hasAny(prov[id.Region], ids) {
			hits = append(hits, t)
		}
	}
	if len(hits) != 1 {
		return nil, nil
	}
	return &hits[0], nil
}

// searchTerms are what to search for: the whole title, its parts around
// ":" and " - ", and its distinctive words (long, not everyday ones).
func searchTerms(title string) []string {
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, x := range out {
			if strings.EqualFold(x, s) {
				return
			}
		}
		out = append(out, s)
	}
	add(title)
	for _, sep := range []string{":", " - ", " – ", " | "} {
		if strings.Contains(title, sep) {
			for _, p := range strings.Split(title, sep) {
				add(p)
			}
		}
	}
	for _, w := range strings.FieldsFunc(title, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) >= 7 && !commonWord[strings.ToLower(w)] {
			add(w)
		}
	}
	return out
}

// sharesTerm: the candidate's name really contains the searched words
// (TMDB's search is loose).
func sharesTerm(t Title, q string) bool {
	nq := store.NormalizeTitle(q)
	for _, n := range []string{t.Title, t.OriginalTitle} {
		if nq != "" && strings.Contains(" "+store.NormalizeTitle(n)+" ", " "+nq+" ") {
			return true
		}
	}
	return false
}

func hasAny(have, want []int) bool {
	for _, h := range have {
		for _, w := range want {
			if h == w {
				return true
			}
		}
	}
	return false
}

// commonWord: long words that name nothing on their own.
var commonWord = map[string]bool{
	"episode": true, "episodio": true, "capitulo": true, "capítulo": true, "temporada": true, "season": true,
	"asesinato": true, "asesinatos": true, "documental": true, "documentary": true,
	"extremo": true, "extreme": true, "historia": true, "history": true, "special": true, "especial": true,
	"película": true, "pelicula": true, "original": true, "version": true, "versión": true, "trailer": true,
	"misterio": true, "mystery": true, "secretos": true, "secrets": true, "nacional": true, "national": true,
}
