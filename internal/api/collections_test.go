//go:build !nosqlite

package api

import (
	"context"
	"testing"

	"kinara/internal/store"
)

func TestFranchiseGrouping(t *testing.T) {
	st, owner, _ := featureServer(t)
	ctx := context.Background()
	lib, _ := st.CreateLibrary(ctx, "Movies", "/m", store.KindMovies)
	fellowship := addMovie(t, st, lib, "The Fellowship of the Ring")
	towers := addMovie(t, st, lib, "The Two Towers")
	king := addMovie(t, st, lib, "The Return of the King")
	heat := addMovie(t, st, lib, "Heat")
	alien := addMovie(t, st, lib, "Alien")
	lotr := &store.Collection{ID: 119, Name: "The Lord of the Rings Collection"}
	for _, id := range []string{fellowship, towers, king} {
		if err := st.SetCollection(ctx, id, lotr); err != nil {
			t.Fatal(err)
		}
	}
	// A franchise with one movie on the server isn't grouped.
	st.SetCollection(ctx, alien, &store.Collection{ID: 8091, Name: "Alien Collection"})
	st.SetCollection(ctx, heat, nil)

	type page struct{ Items []ItemView }
	list := func(q string) []ItemView {
		var p page
		owner.do("GET", "/items?kind=movie&libraryId="+itoa(lib.ID)+q, nil, &p)
		return p.Items
	}
	if got := list(""); len(got) != 5 {
		t.Fatalf("without asking: %d items", len(got))
	}
	got := list("&collections=1")
	if len(got) != 3 {
		t.Fatalf("grouped: %+v", got)
	}
	var card *ItemView
	for i := range got {
		if got[i].Kind == store.ItemCollection {
			card = &got[i]
		}
	}
	if card == nil || card.ID != "c119" || card.Count != 3 || card.Title != lotr.Name {
		t.Fatalf("franchise card %+v", card)
	}
	// Sorted by title: Alien, Heat, Lord of the Rings.
	if got[0].Title != "Alien" || got[1].Title != "Heat" || got[2].ID != "c119" {
		t.Fatalf("order %v %v %v", got[0].Title, got[1].Title, got[2].Title)
	}
	var letters struct{ Letters []store.LetterOffset }
	owner.do("GET", "/items/letters?kind=movie&libraryId="+itoa(lib.ID)+"&collections=1", nil, &letters)
	if len(letters.Letters) != 3 || letters.Letters[2].Letter != "L" || letters.Letters[2].Offset != 2 {
		t.Fatalf("letters %+v", letters.Letters)
	}

	// The franchise page, and a movie pointing to it.
	var detail struct {
		Item  ItemView
		Items []ItemView
	}
	owner.do("GET", "/items/c119", nil, &detail)
	if detail.Item.Kind != store.ItemCollection || len(detail.Items) != 3 {
		t.Fatalf("franchise page %+v", detail)
	}
	var movie struct{ Collection *collectionInfo }
	owner.do("GET", "/items/"+towers, nil, &movie)
	if movie.Collection == nil || movie.Collection.ID != "c119" || movie.Collection.Count != 3 {
		t.Fatalf("movie's franchise %+v", movie.Collection)
	}
	var children struct{ Items []ItemView }
	owner.do("GET", "/items/c119/children", nil, &children)
	if len(children.Items) != 3 {
		t.Fatalf("children %+v", children.Items)
	}

	// The owner can turn grouping off.
	owner.do("PUT", "/settings", map[string]any{"groupCollections": false}, nil)
	if got := list("&collections=1"); len(got) != 5 {
		t.Fatalf("with grouping off: %d items", len(got))
	}
}
