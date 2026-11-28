package identify

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

func TestParseEpisodeRef(t *testing.T) {
	for in, want := range map[string]struct {
		s, e int
		rest string
	}{
		"S02:E21 - Lauren":        {2, 21, "Lauren"},
		"S1 E2":                   {1, 2, ""},
		"Ozark S01E02":            {1, 2, "Ozark"},
		"T1:E2 La caída":          {1, 2, "La caída"},
		"Season 1: Episode 2":     {1, 2, ""},
		"Temporada 3 Episodio 10": {3, 10, ""},
		"1x02 - Pilot":            {1, 2, "Pilot"},
		"Season 2, Ep. 4 · Heist": {2, 4, "Heist"},
	} {
		s, e, rest, ok := parseEpisodeRef(in)
		if !ok || s != want.s || e != want.e || rest != want.rest {
			t.Errorf("%q → S%dE%d %q %v", in, s, e, rest, ok)
		}
	}
	for _, in := range []string{"The Rise of Top G", "Polyester King", "RESUMEN: PSG 2-2 AS ROMA", "Studio 54"} {
		if _, _, _, ok := parseEpisodeRef(in); ok {
			t.Errorf("%q parsed as an episode", in)
		}
	}
}

// autoCatalog holds the shows from your real sessions.
func autoCatalog() *fakeCatalog {
	c := catalog()
	unseamly := Title{Kind: Show, TMDB: 300100, Title: "Unseamly: The Investigation of Peter Nygård", Seasons: []Season{{Number: 1, Episodes: 4}}}
	short := Title{Kind: Show, TMDB: 300101, Title: "Unseamly: The Investigation of Pete", Seasons: []Season{{Number: 1, Episodes: 4}}}
	tate := Title{Kind: Show, TMDB: 343245, Title: "Andrew Tate: Toxic Empire", Year: 2026, Seasons: []Season{{Number: 1, Episodes: 4}}}
	intervention := Title{Kind: Show, TMDB: 1435, Title: "Intervention", Seasons: []Season{{Number: 1, Episodes: 12}, {Number: 2, Episodes: 26}, {Number: 3, Episodes: 15}}}
	fire := Title{Kind: Movie, TMDB: 555, Title: "Missing from Fire Trail Road", Year: 2025}
	for _, x := range []Title{unseamly, short, tate, intervention, fire} {
		c.titles[string(x.Kind)+":"+itoa(x.TMDB)] = x
	}
	c.seasons["300101:1"] = []Episode{{Number: 1, Name: "Polyester King"}, {Number: 2, Name: "Devil in His Eyes"}}
	c.seasons["343245:1"] = []Episode{{Number: 1, Name: "Episode 1"}, {Number: 2, Name: "Episode 2"}}
	c.seasons["1435:2"] = []Episode{{Number: 20, Name: "Laurie & Jessie"}, {Number: 21, Name: "Lauren"}, {Number: 22, Name: "Ryan"}}
	// as TMDB lists it: Tubi calls "Kim" S3E2
	c.seasons["1435:3"] = []Episode{{Number: 1, Name: "Ryan"}, {Number: 2, Name: "Jacob"}, {Number: 3, Name: "Kim"}}
	c.searches["Unseamly: The Investigation of Pete"] = []Title{short}
	c.searches["Andrew Tate: Toxic Empire"] = []Title{tate}
	c.searches["Missing From Fire Trail Road"] = []Title{fire}
	return c
}

func itoa(n int) string { return strconv.Itoa(n) }

func autoRig(t *testing.T) (*Identifier, *store.Store) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Identifier{Catalog: autoCatalog(), Aliases: st, History: st}, st
}

func hbo(title, sub string) *model.Session {
	return &model.Session{App: model.App{ID: "com.wbd.stream"}, Hint: model.Hint{Title: title, Subtitle: sub}}
}

func TestAutoHBOEpisodeByName(t *testing.T) {
	id, _ := autoRig(t)
	it, how, err := id.Auto(context.Background(), store.LocalUserID, hbo("Devil in His Eyes", "Unseamly: The Investigation of Pete"))
	if err != nil || it == nil || it.TMDB != 300101 || it.Season != 1 || it.Episode != 2 || how != ByMatch {
		t.Fatalf("%+v %q %v", it, how, err)
	}
}

func TestAutoNewSeriesGuessesNextEpisode(t *testing.T) {
	id, st := autoRig(t)
	ctx := context.Background()
	it, how, err := id.Auto(ctx, store.LocalUserID, hbo("The Rise of Top G", "Andrew Tate: Toxic Empire"))
	if err != nil || it == nil || it.Episode != 1 || how != ByProgress || !how.Guessed() {
		t.Fatalf("first: %+v %q %v", it, how, err)
	}
	// After E1 is in the history, the next unnamed episode is E2.
	dev, _ := st.EnsureDevice(ctx, store.LocalUserID, "androidtv", "Box", "")
	sess, _ := st.StartSession(ctx, dev, model.App{ID: "com.wbd.stream"}, "", time.Now())
	st.Identify(ctx, sess.ID, []model.Item{*it}, nil, time.Now())
	it, how, _ = id.Auto(ctx, store.LocalUserID, hbo("Episode Two Name", "Andrew Tate: Toxic Empire"))
	if it == nil || it.Episode != 2 || how != ByProgress {
		t.Errorf("second: %+v %q", it, how)
	}
	// TMDB lists two episodes so far; after E2 the guess goes on to E3 anyway.
	sess2, _ := st.StartSession(ctx, dev, model.App{ID: "com.wbd.stream"}, "", time.Now().Add(time.Minute))
	st.Identify(ctx, sess2.ID, []model.Item{*it}, nil, time.Now())
	it, how, _ = id.Auto(ctx, store.LocalUserID, hbo("Third", "Andrew Tate: Toxic Empire"))
	if it == nil || it.Season != 1 || it.Episode != 3 || how != ByProgress {
		t.Errorf("beyond what TMDB lists: %+v %q", it, how)
	}
}

func TestAutoNamedEpisodesButNoMatchAsks(t *testing.T) {
	id, _ := autoRig(t)
	if it, _, _ := id.Auto(context.Background(), store.LocalUserID, hbo("Some Other Name", "Unseamly: The Investigation of Pete")); it != nil {
		t.Errorf("guessed %+v although the show's episodes have names", it)
	}
}

func TestAutoTubiContinuesTheShow(t *testing.T) {
	id, st := autoRig(t)
	ctx := context.Background()
	tubi := func(title string) *model.Session {
		return &model.Session{App: model.App{ID: "com.tubitv"}, Hint: model.Hint{Title: title}}
	}
	if it, _, _ := id.Auto(ctx, store.LocalUserID, tubi("S02:E21 - Lauren")); it != nil {
		t.Errorf("no history yet, still identified %+v", it)
	}
	dev, _ := st.EnsureDevice(ctx, store.LocalUserID, "androidtv", "Box", "")
	sess, _ := st.StartSession(ctx, dev, model.App{ID: "com.tubitv"}, "", time.Now().Add(-time.Hour))
	st.Identify(ctx, sess.ID, []model.Item{{Identity: model.Identity{Kind: model.KindEpisode, Title: "Intervention", TMDB: 1435, Season: 2, Episode: 20}}}, nil, time.Now())
	it, how, err := id.Auto(ctx, store.LocalUserID, tubi("S02:E21 - Lauren"))
	if err != nil || it == nil || it.TMDB != 1435 || it.Episode != 21 || how != ByContinuation {
		t.Fatalf("continuation: %+v %q %v", it, how, err)
	}
	// Tubi numbers "Kim" S3E2; TMDB has it as E3: the name wins.
	if it, _, _ := id.Auto(ctx, store.LocalUserID, tubi("S03:E02 - Kim")); it == nil || it.Season != 3 || it.Episode != 3 {
		t.Errorf("Kim: %+v", it)
	}
	// "Ryan" is in seasons 2 and 3: the app's season decides.
	if it, _, _ := id.Auto(ctx, store.LocalUserID, tubi("S03:E01 - Ryan")); it == nil || it.Season != 3 || it.Episode != 1 {
		t.Errorf("Ryan: %+v", it)
	}
	// The name must match: a different show's episode is left alone.
	if it, _, _ := id.Auto(ctx, store.LocalUserID, tubi("S02:E21 - Somebody Else")); it != nil {
		t.Errorf("name mismatch identified %+v", it)
	}
}

func TestAutoYouTubeFilmStore(t *testing.T) {
	id, _ := autoRig(t)
	ctx := context.Background()
	yt := func(title, sub string) *model.Session {
		return &model.Session{App: model.App{ID: "com.google.android.youtube.tv"}, Hint: model.Hint{Title: title, Subtitle: sub}}
	}
	it, how, err := id.Auto(ctx, store.LocalUserID, yt("Missing From Fire Trail Road", "Películas y series de YouTube"))
	if err != nil || it == nil || it.TMDB != 555 || how != ByMatch {
		t.Fatalf("film store: %+v %q %v", it, how, err)
	}
	if it, _, _ := id.Auto(ctx, store.LocalUserID, yt("Missing From Fire Trail Road", "Some channel")); it != nil {
		t.Errorf("a regular YouTube video identified: %+v", it)
	}
}

func TestAutoHBOEpisodeNameWordedDifferently(t *testing.T) {
	id, _ := autoRig(t)
	it, how, err := id.Auto(context.Background(), store.LocalUserID, hbo("Devil in His Own Eyes", "Unseamly: The Investigation of Pete"))
	if err != nil || it == nil || it.Season != 1 || it.Episode != 2 || how != ByMatch {
		t.Fatalf("%+v %q %v", it, how, err)
	}
}

func TestTitleSimilarity(t *testing.T) {
	if r := titleSimilarity("in your own time", "in your time"); r < 0.8 {
		t.Errorf("subset: %v", r)
	}
	if r := titleSimilarity("fire and water", "mental evidence"); r >= 0.8 {
		t.Errorf("unrelated: %v", r)
	}
}

func TestAutoAliasForAShowNameGuessesTheEpisode(t *testing.T) {
	id, st := autoRig(t)
	ctx := context.Background()
	disney := &model.Session{App: model.App{ID: "com.disney.disneyplus"}, Hint: model.Hint{Title: "Looksmaxxing extremo"}}
	if it, _, _ := id.Auto(ctx, store.LocalUserID, disney); it != nil {
		t.Fatalf("identified before any alias: %+v", it)
	}
	// you identified it once as the show and asked to remember
	st.SaveAlias(ctx, model.Alias{UserID: store.LocalUserID, AppID: "com.disney.disneyplus", MatchTitle: "Looksmaxxing extremo",
		Identity: model.Identity{Kind: model.KindEpisode, Title: "Unseamly", TMDB: 300101}}, time.Now())
	dev, _ := st.EnsureDevice(ctx, store.LocalUserID, "androidtv", "Box", "")
	sess, _ := st.StartSession(ctx, dev, model.App{ID: "com.disney.disneyplus"}, "", time.Now().Add(-time.Hour))
	st.Identify(ctx, sess.ID, []model.Item{{Identity: model.Identity{Kind: model.KindEpisode, Title: "Unseamly", TMDB: 300101, Season: 1, Episode: 1}}}, nil, time.Now())
	it, how, err := id.Auto(ctx, store.LocalUserID, disney)
	if err != nil || it == nil || it.TMDB != 300101 || it.Episode != 2 || how != ByProgress {
		t.Errorf("%+v %q %v", it, how, err)
	}
}
