// Package grammar is a deliberately small part-of-speech model for judging
// whether one transcribed piece can plausibly follow another.
//
// It only ever looks at the word on each side of a junction. A dictionary of
// closed-class words (determiners, pronouns, prepositions, auxiliaries…) plus
// suffix rules for open-class words is enough to rule out joins like
// "the | went" or "of | is", which are what make a stitched sentence sound
// ungrammatical. Whisper's transcripts of old, noisy recordings are imperfect
// anyway, so a statistical tagger would add weight without much benefit.
package grammar

import (
	"strings"
	"unicode"
)

// Tag is a set of possible parts of speech for a word. Words are often
// ambiguous ("that" is a determiner, pronoun or conjunction), so tags are
// bit sets and a junction is allowed if any reading of each side fits.
// The zero Tag means unknown and fits anywhere.
type Tag uint16

const (
	Det  Tag = 1 << iota // the, a, this, my
	Pron                 // I, you, him, someone
	Prep                 // of, in, with
	Part                 // infinitive "to"
	Conj                 // and, but, because
	Aux                  // is, have, can, don't
	Verb                 // went, find, running
	Adv                  // not, very, quickly
	Adj                  // old, happy, dangerous
	Noun                 // man, boat, Poland
	Num                  // two, 1941
	Intj                 // yes, oh, well
)

const allTags = Det | Pron | Prep | Part | Conj | Aux | Verb | Adv | Adj | Noun | Num | Intj

// follows maps each single part of speech to the parts of speech that may
// come directly after it.
var follows = map[Tag]Tag{
	Det:  Adj | Noun | Num | Adv,
	Pron: Verb | Aux | Adv | Prep | Part | Conj | Det | Pron | Noun,
	Prep: Det | Pron | Noun | Adj | Num | Verb,
	Part: Verb | Adv,
	Conj: allTags &^ Intj,
	Aux:  Verb | Adv | Pron | Det | Adj | Noun | Prep | Part | Num,
	Verb: Det | Pron | Noun | Adj | Adv | Prep | Part | Conj | Num | Verb,
	Adv:  allTags &^ Intj,
	Adj:  Noun | Adj | Conj | Prep | Part,
	Noun: Verb | Aux | Prep | Part | Conj | Noun | Adv | Pron,
	Num:  Noun | Adj | Prep | Conj | Num,
	Intj: allTags,
}

// CanFollow reports whether a word tagged next may directly follow a word
// tagged prev. Unknown tags (zero) fit anywhere.
func CanFollow(prev, next Tag) bool {
	if prev == 0 || next == 0 {
		return true
	}
	for bit := Tag(1); bit <= Intj; bit <<= 1 {
		if prev&bit != 0 && follows[bit]&next != 0 {
			return true
		}
	}
	return false
}

// Of returns the possible parts of speech for a single word as Whisper
// writes it (any case, with surrounding punctuation).
func Of(word string) Tag {
	w := Normalize(word)
	if w == "" {
		return 0
	}
	if t, ok := lexicon[w]; ok {
		return t
	}
	if w[0] >= '0' && w[0] <= '9' {
		return Num
	}
	return bySuffix(w)
}

// Normalize lowercases a word, straightens curly apostrophes and strips
// surrounding punctuation.
func Normalize(word string) string {
	w := strings.ToLower(strings.ReplaceAll(word, "’", "'"))
	return strings.Trim(w, ` .,;:!?"()[]-—…'`)
}

// CanEnd reports whether a word tagged t can plausibly end a sentence — not
// a determiner, preposition, conjunction or bare "to".
func CanEnd(t Tag) bool {
	return t == 0 || t&(Pron|Verb|Adv|Adj|Noun|Num|Intj) != 0
}

// Capitalized reports whether a transcribed word starts with a capital
// letter other than the pronoun "I". Whisper capitalises sentence starts
// (and proper nouns, which make acceptable sentence starts too).
func Capitalized(word string) bool {
	w := Normalize(word)
	if w == "i" || strings.HasPrefix(w, "i'") {
		return false
	}
	for _, r := range strings.TrimLeft(word, ` "'(`) {
		return unicode.IsUpper(r)
	}
	return false
}

// EndsSentence reports whether a transcribed word carries sentence-final
// punctuation.
func EndsSentence(word string) bool {
	w := strings.TrimRight(strings.TrimSpace(word), `"')]`)
	return strings.HasSuffix(w, ".") || strings.HasSuffix(w, "?") || strings.HasSuffix(w, "!")
}

func bySuffix(w string) Tag {
	switch {
	case strings.HasSuffix(w, "ly"):
		return Adv | Adj // quickly, lovely
	case strings.HasSuffix(w, "ing"):
		return Verb | Noun | Adj
	case strings.HasSuffix(w, "ed"):
		return Verb | Adj
	case hasAnySuffix(w, "tion", "sion", "ment", "ness", "ity", "ship", "hood", "ism"):
		return Noun
	case hasAnySuffix(w, "ous", "ful", "less", "able", "ible", "ive", "ic", "ish", "al"):
		return Adj
	case strings.HasSuffix(w, "s"):
		return Noun | Verb // boats, finds
	}
	return Noun | Verb // most unknown words are nouns; bare verbs are common too
}

func hasAnySuffix(w string, suffixes ...string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(w, s) {
			return true
		}
	}
	return false
}

// lexicon covers the closed-class words that decide most junctions, plus
// common irregular verbs and adjectives the suffix rules can't recognise.
var lexicon = map[string]Tag{}

func init() {
	add := func(t Tag, words string) {
		for _, w := range strings.Fields(words) {
			lexicon[w] |= t
		}
	}
	add(Det, `the a an this that these those my your his her its our their some any no
		every each all both either neither another such what which whose much many few several`)
	add(Pron, `i you he she it we they me him her us them myself yourself himself herself
		itself ourselves themselves mine yours hers ours theirs who whom whoever whatever
		something anything nothing everything someone anyone everyone nobody somebody anybody
		everybody this that these those what which one there`)
	add(Pron|Aux, `i'm you're he's she's it's we're they're i've you've we've they've i'd you'd
		he'd she'd we'd they'd i'll you'll he'll she'll we'll they'll that's there's what's
		who's let's here's`)
	add(Prep, `of in on at by for with about against between into through during before after
		above below to from up down out off over under near since until upon within without
		toward towards across along around behind beyond beside like than`)
	add(Part, `to`)
	add(Conj, `and but or nor so yet because although though while if unless whether as
		that when where then`)
	add(Aux, `am is are was were be been being have has had having do does did will would
		shall should can could may might must isn't aren't wasn't weren't don't doesn't
		didn't won't wouldn't can't couldn't shouldn't haven't hasn't hadn't ain't gonna`)
	add(Adv, `not very too also just only even still already always never often sometimes now
		then here there how why when where again soon quite rather really almost well back
		away ever perhaps maybe once yet so`)
	add(Intj, `yes no oh ah well hey hello okay ok please thanks alright gee say hmm goodbye
		yeah sir madam`)
	add(Num, `two three four five six seven eight nine ten eleven twelve twenty thirty forty
		fifty hundred thousand million first second third one`)
	add(Verb, `go went gone goes get got gets make made say said says know knew known think
		thought take took taken see saw seen come came want look use give gave given find
		found tell told ask work seem feel felt try leave left call keep kept let begin began
		help show hear heard play run ran move live believe bring brought happen write wrote
		sit sat stand stood lose lost pay paid meet met put mean meant set learn change lead
		led understand understood watch follow stop speak spoke read spend spent grow grew
		open walk win won wait love remember die send sent build stay fall fell cut reach
		kill remain buy bought eat ate drink drank sleep slept need like hope wish sing sang
		fly flew drive drove ride rode catch caught teach taught fight fought forget forgot
		hold held become became sell sold`)
	add(Adj, `good new old great big small little long high young right large next early
		important bad same able last other own sure free full real best better dark dead
		poor happy sorry ready fine nice dear true whole strange certain afraid alone late
		short cold hot hard easy wrong black white red green blue beautiful terrible awful
		wonderful lovely quiet open kind fair rich`)
}
