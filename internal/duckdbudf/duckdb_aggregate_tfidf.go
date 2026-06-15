package duckdbudf

import (
	"math"
	"sort"

	"github.com/goccy/googlesqlite/internal/functions/helper"
	"github.com/goccy/googlesqlite/internal/value"
)

// newTfIdfAggregator adapts the BigQuery TF_IDF analytic to a plain
// helper.Aggregator so the C aggregate bridge can register it. DuckDB runs any
// registered aggregate as a window function under OVER (), which is how the
// tf_idf spec invokes it: `TO_JSON_STRING(TF_IDF(t) OVER ()) ... LIMIT 1`. With
// an empty OVER (), the window is the whole partition, so every row sees the
// same corpus; the query then keeps only the first row.
//
// The aggregate therefore collects every document's token array across the
// group and, at finalize, emits the per-row TF-IDF scores for the FIRST
// document (the row the LIMIT 1 surfaces). The scoring is the same formula the
// window body internal/functions/text_analysis.TF_IDF uses:
//
//	TF(t, d)  = count(t in d) / |d|
//	IDF(t, N) = log((1 + N) / (1 + df(t))) + 1
//	score     = TF * IDF
//
// max_distinct_tokens / frequency_threshold use the body's defaults (32000 /
// 0). The spec exercises only the default form; the optional INT64 args are not
// passed, so the single-arg aggregate registration matches the call's arity.
func newTfIdfAggregator() *helper.Aggregator {
	var docs [][]string
	return helper.NewAggregator(
		func(args []value.Value, _ *helper.Option) error {
			// A NULL document contributes an empty token list (it still counts
			// toward N), matching the window body's NULL-doc handling.
			if len(args) == 0 || args[0] == nil {
				docs = append(docs, nil)
				return nil
			}
			arr, err := args[0].ToArray()
			if err != nil {
				return err
			}
			toks := make([]string, 0, len(arr.Values))
			for _, tok := range arr.Values {
				if tok == nil {
					continue
				}
				s, err := tok.ToString()
				if err != nil {
					return err
				}
				toks = append(toks, s)
			}
			docs = append(docs, toks)
			return nil
		},
		func() (value.Value, error) {
			return tfIdfFirstRow(docs), nil
		},
	)
}

// tfIdfFirstRow computes the TF-IDF score array for the first document in the
// corpus, sorted by score descending then term ascending. Returns an empty
// ARRAY when the corpus is empty or the first document has no tokens.
func tfIdfFirstRow(docs [][]string) value.Value {
	if len(docs) == 0 {
		return &value.ArrayValue{}
	}
	// Corpus-wide document frequency: how many documents contain each term.
	df := map[string]int64{}
	for _, doc := range docs {
		seen := map[string]struct{}{}
		for _, t := range doc {
			seen[t] = struct{}{}
		}
		for t := range seen {
			df[t]++
		}
	}
	nDocs := int64(len(docs))

	// Per-term counts for the first document.
	first := docs[0]
	counts := map[string]int64{}
	var total int64
	for _, t := range first {
		counts[t]++
		total++
	}
	if total == 0 {
		return &value.ArrayValue{}
	}

	type scored struct {
		term  string
		score float64
	}
	rows := make([]scored, 0, len(counts))
	for term, count := range counts {
		tf := float64(count) / float64(total)
		idf := math.Log(float64(1+nDocs)/float64(1+df[term])) + 1
		rows = append(rows, scored{term, tf * idf})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].score != rows[j].score {
			return rows[i].score > rows[j].score
		}
		return rows[i].term < rows[j].term
	})

	out := &value.ArrayValue{Values: make([]value.Value, 0, len(rows))}
	for _, r := range rows {
		out.Values = append(out.Values, &value.StructValue{
			Keys:   []string{"term", "score"},
			Values: []value.Value{value.StringValue(r.term), value.FloatValue(r.score)},
			M: map[string]value.Value{
				"term":  value.StringValue(r.term),
				"score": value.FloatValue(r.score),
			},
		})
	}
	return out
}
