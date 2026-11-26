package identify

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

// fakeCatalog is TMDB in memory.
type fakeCatalog struct {
	titles   map[string]Title // "movie:1", "tv:2"
	seasons  map[string][]Episode
	searches map[string][]Title
	calls    int
}

func (f *fakeCatalog) Search(_ context.Context, q string) ([]Title, error) {
	f.calls++
	return f.searches[q], nil
}

func (f *fakeCatalog) Get(_ context.Context, k Kind, id int) (Title, error) {
	f.calls++
	t, ok := f.titles[fmt.Sprintf("%s:%d", k, id)]
	if !ok {
		return Title{}, ErrNotFound
	}
	return t, nil
}

func (f *fakeCatalog) Season(_ context.Context, show, season int) ([]Episode, error) {
	f.calls++
	eps, ok := f.seasons[fmt.Sprintf("%d:%d", show, season)]
	if !ok {
		return nil, ErrNotFound
	}
	return eps, nil
}

func (f *fakeCatalog) Find(_ context.Context, id, source string) ([]Title, error) {
	f.calls++
	if source == "imdb_id" && id == "tt1160419" {
		return []Title{f.titles["movie:438631"]}, nil
	}
	return nil, nil
}

func catalog() *fakeCatalog {
	dune := Title{Kind: Movie, TMDB: 438631, Title: "Dune", Year: 2021, IMDb: "tt1160419", Runtime: 155, Poster: "p"}
	dune84 := Title{Kind: Movie, TMDB: 841, Title: "Dune", Year: 1984, Runtime: 137}
	curse := Title{Kind: Show, TMDB: 114655, Title: "The Curse", Year: 2023, IMDb: "tt13623608", TVDB: 377278, Runtime: 60,
		Seasons: []Season{{0, 1}, {1, 10}, {2, 0}, {3, 8}}}
	enfants := Title{Kind: Movie, TMDB: 1234, Title: "Out of Love", OriginalTitle: "Les enfants vont bien", Year: 2025, Runtime: 111}
	return &fakeCatalog{
		titles: map[string]Title{"movie:9": {Kind: Movie, TMDB: 9, Title: "The Curse", Year: 1987}, "movie:438631": dune, "movie:841": dune84, "tv:114655": curse, "movie:1234": enfants},
		seasons: map[string][]Episode{
			"114655:1": {{Number: 1, Name: "Land of Enchantment", Runtime: 62}, {Number: 2, Name: "Pressure Washer"}, {Number: 10, Name: "Green Queen", Runtime: 70}},
			"114655:3": {{Number: 1, Name: "New", Runtime: 55}},
		},
		searches: map[string][]Title{
			"Dune":                  {dune, dune84},
			"The Curse":             {Title{Kind: Movie, TMDB: 9, Title: "The Curse", Year: 1987}, curse},
			"Les enfants vont bien": {enfants},
		},
	}
}

func TestParseRef(t *testing.T) {
	for q, want := range map[string]Ref{
		"https://www.imdb.com/title/tt1160419/":          {Source: "imdb", ID: "tt1160419"},
		"https://www.themoviedb.org/tv/114655-the-curse": {Source: "tmdb", Kind: Show, ID: "114655"},
		"tmdb:movie:438631":                              {Source: "tmdb", Kind: Movie, ID: "438631"},
		"https://thetvdb.com/?tab=series&id=377278":      {Source: "tvdb", ID: "377278"},
		"tvdb:377278": {Source: "tvdb", ID: "377278"},
	} {
		if got, ok := ParseRef(q); !ok || got != want {
			t.Errorf("ParseRef(%q) = %+v, %v; want %+v", q, got, ok, want)
		}
	}
	if _, ok := ParseRef("Dune"); ok {
		t.Error("a plain title parsed as a ref")
	}
}

func TestSearchByRefAndTitle(t *testing.T) {
	id := &Identifier{Catalog: catalog()}
	ctx := context.Background()
	if got, _ := id.Search(ctx, "tt1160419"); len(got) != 1 || got[0].TMDB != 438631 {
		t.Errorf("imdb ref: %+v", got)
	}
	if got, _ := id.Search(ctx, "tmdb:tv:114655"); len(got) != 1 || got[0].Title != "The Curse" {
		t.Errorf("tmdb ref: %+v", got)
	}
	if got, err := id.Search(ctx, "tmdb:movie:1"); err != nil || len(got) != 0 {
		t.Errorf("missing tmdb ref: %+v %v", got, err)
	}
	if got, _ := id.Search(ctx, "Dune"); len(got) != 2 {
		t.Errorf("title search: %+v", got)
	}
}

func TestItemEpisode(t *testing.T) {
	id := &Identifier{Catalog: catalog()}
	ctx := context.Background()
	it, err := id.Item(ctx, Show, 114655, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if it.Kind != model.KindEpisode || it.Title != "The Curse" || it.EpisodeTitle != "Land of Enchantment" ||
		it.RuntimeMinutes != 62 || it.RuntimeEstimated || it.IMDb != "tt13623608" || it.TVDB != 377278 || it.TMDB != 114655 {
		t.Fatalf("item %+v", it)
	}
	// No runtime for this episode yet: the show's usual length, estimated.
	it, _ = id.Item(ctx, Show, 114655, 1, 2)
	if it.RuntimeMinutes != 60 || !it.RuntimeEstimated || it.EpisodeTitle != "Pressure Washer" {
		t.Errorf("estimated item %+v", it)
	}
	if _, err := id.Item(ctx, Show, 114655, 1, 0); !errors.Is(err, ErrNeedsEpisode) {
		t.Errorf("show without an episode: %v", err)
	}
	mv, _ := id.Item(ctx, Movie, 438631, 0, 0)
	if mv.Kind != model.KindMovie || mv.RuntimeMinutes != 155 || mv.Season != 0 {
		t.Errorf("movie %+v", mv)
	}
}

func TestNext(t *testing.T) {
	id := &Identifier{Catalog: catalog()}
	ctx := context.Background()
	ep := func(s, e int) model.Item {
		return model.Item{Identity: model.Identity{Kind: model.KindEpisode, Title: "The Curse", TMDB: 114655, Season: s, Episode: e}}
	}
	if n, err := id.Next(ctx, ep(1, 1)); err != nil || n.Season != 1 || n.Episode != 2 || n.EpisodeTitle != "Pressure Washer" {
		t.Errorf("after S1E1: %+v %v", n, err)
	}
	// Last of season 1; season 2 is listed with no episodes: on to season 3.
	if n, err := id.Next(ctx, ep(1, 10)); err != nil || n.Season != 3 || n.Episode != 1 || n.EpisodeTitle != "New" {
		t.Errorf("after S1E10: %+v %v", n, err)
	}
	if _, err := id.Next(ctx, ep(3, 8)); !errors.Is(err, ErrNotFound) {
		t.Errorf("after the last episode: %v", err)
	}
}

func session(app, title string, s, e int) *model.Session {
	return &model.Session{App: model.App{ID: app}, Hint: model.Hint{Title: title, Season: s, Episode: e}}
}

func TestAutoExactMatch(t *testing.T) {
	cat := catalog()
	id := &Identifier{Catalog: cat}
	ctx := context.Background()
	// The device's title is the original one; TMDB lists it under its English title.
	it, how, err := id.Auto(ctx, 1, session("netflix", "Les enfants vont bien", 0, 0))
	if err != nil || it == nil || it.TMDB != 1234 || how != ByMatch {
		t.Fatalf("auto %+v %q %v", it, how, err)
	}
	// Episode numbers pick the show over a movie of the same name.
	it, how, _ = id.Auto(ctx, 1, session("com.hbo.hbonow", "The Curse", 1, 10))
	if it == nil || it.TMDB != 114655 || it.Episode != 10 || it.EpisodeTitle != "Green Queen" || how != ByMatch {
		t.Errorf("episode auto %+v %q", it, how)
	}
}

func TestAutoLeavesDoubtsToTheUser(t *testing.T) {
	cat := catalog()
	id := &Identifier{Catalog: cat}
	ctx := context.Background()
	for name, s := range map[string]*model.Session{
		"two films called Dune":       session("netflix", "Dune", 0, 0),
		"no title published":          session("netflix", "", 0, 0),
		"title without a match":       session("netflix", "Something else", 0, 0),
		"show with no episode number": session("netflix", "The Curse", 0, 0), // only the 1987 film matches as a movie
	} {
		it, _, err := id.Auto(ctx, 1, s)
		if name == "show with no episode number" {
			// The 1987 film is the only movie by that name: that is a match.
			if it == nil || it.TMDB != 9 {
				t.Errorf("%s: %+v", name, it)
			}
			continue
		}
		if err != nil || it != nil {
			t.Errorf("%s: identified %+v (err %v), want left to the user", name, it, err)
		}
	}
	before := cat.calls
	if it, _, _ := id.Auto(ctx, 1, session("com.google.android.youtube.tv", "Les enfants vont bien", 0, 0)); it != nil {
		t.Errorf("YouTube video guessed as %+v", it)
	}
	if cat.calls != before {
		t.Error("searched TMDB for a YouTube title")
	}
}

func TestAutoAliasWins(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()
	// "Dune" alone is ambiguous; the user said that on this app it is the 1984 film.
	if _, err := st.SaveAlias(ctx, model.Alias{UserID: store.LocalUserID, AppID: "netflix", MatchTitle: "Dune",
		Identity: model.Identity{Kind: model.KindMovie, Title: "Dune", Year: 1984, TMDB: 841}}, now); err != nil {
		t.Fatal(err)
	}
	// And that "Curse" on HBO is the show.
	st.SaveAlias(ctx, model.Alias{UserID: store.LocalUserID, MatchTitle: "Curse",
		Identity: model.Identity{Kind: model.KindEpisode, Title: "The Curse", TMDB: 114655}}, now)
	id := &Identifier{Catalog: catalog(), Aliases: st}
	it, how, err := id.Auto(ctx, store.LocalUserID, session("netflix", "DUNE", 0, 0))
	if err != nil || it == nil || it.TMDB != 841 || how != ByAlias {
		t.Fatalf("alias %+v %q %v", it, how, err)
	}
	it, how, _ = id.Auto(ctx, store.LocalUserID, session("hbo", "Curse", 1, 1))
	if it == nil || it.Kind != model.KindEpisode || it.Episode != 1 || how != ByAlias {
		t.Errorf("show alias %+v %q", it, how)
	}
	// A show alias without episode numbers cannot become an item: left to the user.
	if it, _, _ := id.Auto(ctx, store.LocalUserID, session("hbo", "Curse", 0, 0)); it != nil {
		t.Errorf("show alias without episode: %+v", it)
	}
	// Aliases also work for apps where titles are never guessed.
	st.SaveAlias(ctx, model.Alias{UserID: store.LocalUserID, AppID: "com.google.android.youtube.tv", MatchTitle: "Dune full movie",
		Identity: model.Identity{Kind: model.KindMovie, Title: "Dune", TMDB: 438631}}, now)
	if it, how, _ := id.Auto(ctx, store.LocalUserID, session("com.google.android.youtube.tv", "Dune full movie", 0, 0)); it == nil || how != ByAlias {
		t.Errorf("YouTube alias %+v %q", it, how)
	}
}
