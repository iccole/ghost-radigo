package grammar

import "testing"

func TestOf(t *testing.T) {
	tests := []struct {
		word string
		want Tag // must include all of these
	}{
		{" The", Det},
		{"boat,", Noun},
		{"Poland?", Noun},
		{"went", Verb},
		{"I’m", Pron | Aux},
		{"quickly", Adv},
		{"running", Verb},
		{"dangerous", Adj},
		{"1941", Num},
		{"that", Det | Pron | Conj},
	}
	for _, tt := range tests {
		if got := Of(tt.word); got&tt.want != tt.want {
			t.Errorf("Of(%q) = %b, want it to include %b", tt.word, got, tt.want)
		}
	}
	if got := Of(" ... "); got != 0 {
		t.Errorf("Of(punctuation) = %b, want unknown", got)
	}
}

func TestCanFollow(t *testing.T) {
	tests := []struct {
		prev, next string
		want       bool
	}{
		{"the", "old", true},
		{"the", "boat", true},
		{"the", "went", false},
		{"of", "is", false},
		{"him", "the", true}, // "give him | the book"
		{"find", "the", true},
		{"old", "the", false},
		{"boat", "went", true},
		{"to", "find", true},
		{"and", "then", true},
		{"yes,", "anything", true},
	}
	for _, tt := range tests {
		if got := CanFollow(Of(tt.prev), Of(tt.next)); got != tt.want {
			t.Errorf("CanFollow(%q, %q) = %v, want %v", tt.prev, tt.next, got, tt.want)
		}
	}
	if !CanFollow(0, Of("the")) || !CanFollow(Of("the"), 0) {
		t.Error("unknown tags must fit anywhere")
	}
}

func TestCanEnd(t *testing.T) {
	for word, want := range map[string]bool{"boat": true, "him": true, "the": false, "of": false, "and": false} {
		if got := CanEnd(Of(word)); got != want {
			t.Errorf("CanEnd(%q) = %v, want %v", word, got, want)
		}
	}
}

func TestCapitalized(t *testing.T) {
	for word, want := range map[string]bool{" Do": true, `"Well,`: true, "I": false, "I'm": false, "you": false} {
		if got := Capitalized(word); got != want {
			t.Errorf("Capitalized(%q) = %v, want %v", word, got, want)
		}
	}
}

func TestEndsSentence(t *testing.T) {
	for word, want := range map[string]bool{
		" English.": true,
		" Poland?":  true,
		` "Stop!"`:  true,
		" Well,":    false,
		" now":      false,
	} {
		if got := EndsSentence(word); got != want {
			t.Errorf("EndsSentence(%q) = %v, want %v", word, got, want)
		}
	}
}
