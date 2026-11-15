// cogmem - Cognitive Memory
// License: MIT

package store

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// maxDomainsPerKeyword is how many OTHER active domains may already route on a
// phrase before it is refused as too generic for one more: a phrase shared that
// widely no longer says which domain is relevant.
const maxDomainsPerKeyword = 3

// Rejection reasons, phrased for showing to the agent.
const reasonTooCommon = "too common"

// KeywordRejection is one keyword phrase that was refused, and why.
type KeywordRejection struct {
	Phrase string
	Reason string // short, e.g. "too common" or "already used by 3 domains"
}

// CheckKeywords splits phrases (normalized and deduplicated as the store keeps
// them) into those that may be keyword triggers of domainID and those refused
// as generic. A phrase is generic when every word in it is a common English
// word (keywordStoplist: "good morning", "today"), or when it is already a
// keyword — agent's or consolidation's — of maxDomainsPerKeyword or more other
// active domains. Pass "" as domainID for a domain not created yet.
//
// A phrase domainID already routes on is exempt from the shared-use rule only:
// it is not refused because other domains took it up since, but a common word
// is refused wherever it is, so one stored before this check existed is dropped
// the next time the list is set.
func (s *Store) CheckKeywords(ctx context.Context, q DBTX, domainID string, phrases []string) ([]string, []KeywordRejection, error) {
	norm := splitKeywords(strings.Join(phrases, ","))
	if len(norm) == 0 {
		return nil, nil, nil
	}
	uses, own, err := s.keywordUses(ctx, q, domainID)
	if err != nil {
		return nil, nil, err
	}
	var accepted []string
	var rejected []KeywordRejection
	for _, p := range norm {
		switch {
		case commonPhrase(p):
			rejected = append(rejected, KeywordRejection{Phrase: p, Reason: reasonTooCommon})
		case !own[p] && uses[p] >= maxDomainsPerKeyword:
			rejected = append(rejected, KeywordRejection{
				Phrase: p, Reason: fmt.Sprintf("already used by %d domains", uses[p]),
			})
		default:
			accepted = append(accepted, p)
		}
	}
	return accepted, rejected, nil
}

// keywordUses counts, per phrase, the active domains other than domainID that
// route on it, and returns the phrases domainID itself routes on.
func (s *Store) keywordUses(ctx context.Context, q DBTX, domainID string) (map[string]int, map[string]bool, error) {
	domains, err := s.ListDomains(ctx, q, StatusActive)
	if err != nil {
		return nil, nil, fmt.Errorf("cogmem: count keyword use: %w", err)
	}
	uses := map[string]int{}
	own := map[string]bool{}
	for _, d := range domains {
		if d.ID == domainID {
			for _, p := range d.KeywordPhrases() {
				own[p] = true
			}
			continue
		}
		for _, p := range d.KeywordPhrases() {
			uses[p]++
		}
	}
	return uses, own, nil
}

// KeywordPatch computes the change that replaces the effective keyword list of
// domain id — the one list the agent sees, its own phrases plus those added
// automatically — with phrases, as a patch (KeywordTriggers, AutoKeywords and
// BlockedKeywords set) plus the refused phrases:
//
//   - a given phrase consolidation added stays consolidation's;
//   - any other accepted phrase becomes the agent's, which unblocks it if the
//     agent had removed it from consolidation's list before;
//   - a consolidation phrase left out is removed and blocked, so consolidation
//     cannot add it back.
//
// Phrases are checked as by CheckKeywords; refused ones are not in the patch.
// Apply the patch with UpdateDomain in the same transaction (WithTx), so the
// read, the check and the write see one state and an edit that also changes
// other fields bumps the version once.
func (s *Store) KeywordPatch(ctx context.Context, q DBTX, id string, phrases []string) (UpdateDomainParams, []KeywordRejection, error) {
	cur, err := s.GetDomain(ctx, q, id, false)
	if err != nil {
		return UpdateDomainParams{}, nil, err
	}
	given := splitKeywords(strings.Join(phrases, ","))
	accepted, rejected, err := s.CheckKeywords(ctx, q, id, given)
	if err != nil {
		return UpdateDomainParams{}, nil, err
	}
	givenSet, acceptedSet := setOf(given), setOf(accepted)
	curAuto := cur.AutoKeywordPhrases()
	autoSet := setOf(curAuto)

	var agent, auto, newlyBlocked []string
	for _, p := range curAuto {
		switch {
		case acceptedSet[p]:
			auto = append(auto, p)
		case !givenSet[p]:
			newlyBlocked = append(newlyBlocked, p)
		}
	}
	for _, p := range accepted {
		if !autoSet[p] {
			agent = append(agent, p)
		}
	}
	var blocked []string
	for _, p := range cur.BlockedKeywordPhrases() {
		if !acceptedSet[p] {
			blocked = append(blocked, p)
		}
	}
	blocked = append(blocked, newlyBlocked...)

	agentStr, autoStr, blockedStr := strings.Join(agent, ","), strings.Join(auto, ","), strings.Join(blocked, ",")
	return UpdateDomainParams{
		KeywordTriggers: &agentStr,
		AutoKeywords:    &autoStr,
		BlockedKeywords: &blockedStr,
	}, rejected, nil
}

func setOf(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// commonPhrase reports whether every word of phrase is in keywordStoplist. A
// phrase with no words at all (punctuation only) counts as common: it has
// nothing distinctive to match on.
func commonPhrase(phrase string) bool {
	for _, w := range phraseWords(phrase) {
		if !keywordStoplist[w] {
			return false
		}
	}
	return true
}

// phraseWords splits a lowercase phrase into words: runs of letters, digits and
// apostrophes, with apostrophes at either end trimmed, so "don't" is one word.
func phraseWords(phrase string) []string {
	phrase = strings.ReplaceAll(phrase, "’", "'")
	fields := strings.FieldsFunc(phrase, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
	out := fields[:0]
	for _, f := range fields {
		if f = strings.Trim(f, "'"); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// keywordStoplist holds common English words that say nothing about which
// domain is relevant: function words, plus the conversational and time words
// that appear in ordinary messages on any topic. A keyword made only of these
// would load its domain on nearly every turn.
var keywordStoplist = func() map[string]bool {
	words := []string{
		// Articles, conjunctions, prepositions.
		"a", "an", "the", "and", "or", "but", "so", "yet", "if", "then", "else",
		"than", "because", "as", "at", "by", "for", "from", "in", "into", "of", "off",
		"on", "out", "over", "to", "up", "down", "with", "without", "about",
		"after", "before", "between", "during", "through", "under",
		"until", "while", "since",
		// Pronouns and determiners.
		"i", "me", "my", "mine", "myself", "we", "us", "our", "ours", "you", "your",
		"yours", "he", "him", "his", "she", "her", "hers", "it", "its", "they", "them",
		"their", "this", "that", "these", "those", "who",
		"what", "which", "where", "when", "why", "how", "all", "any", "some", "each",
		"every", "both", "few", "many", "much", "more", "most",
		"other", "another", "such", "same", "own", "one", "thing", "things",
		"something", "anything", "nothing", "everything", "someone",
		"stuff", "lot", "bit", "way",
		// Auxiliaries and common verbs.
		"be", "am", "is", "are", "was", "were", "been", "being", "have", "has", "had",
		"do", "does", "did", "done", "will", "would",
		"should", "can", "could", "may", "might", "must", "get", "got", "go", "going",
		"make", "made", "take", "know", "think", "want", "need", "see", "look",
		"let", "say", "said", "tell", "ask", "use", "try", "come", "give", "like",
		// Contractions.
		"i'm", "i'll", "you're", "it's", "that's", "let's", "don't", "can't", "won't",
		// Adverbs and fillers.
		"not", "no", "yes", "too", "very", "just", "also", "only", "even", "still",
		"already", "again", "ever", "never", "always", "often", "sometimes", "here",
		"there", "really", "maybe", "probably", "actually", "anyway", "well", "right",
		"back", "new", "old", "first", "last", "next",
		// Conversational.
		"ok", "okay", "yeah", "hi", "hello", "hey", "thanks", "thank",
		"please", "sorry", "sure", "fine", "good", "great", "nice", "cool",
		"help", "question", "message",
		// Time.
		"time", "today", "tomorrow", "yesterday", "tonight", "morning", "afternoon",
		"evening", "night", "day", "days", "week", "weeks", "weekend", "month",
		"months", "year", "years", "hour", "hours", "minute", "minutes", "moment",
		"now", "later", "soon", "earlier", "monday", "tuesday", "wednesday",
		"thursday", "friday", "saturday", "sunday",
	}
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}()
