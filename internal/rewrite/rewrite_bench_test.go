package rewrite_test

import (
	"testing"

	"domains.lst/sub-preprocessor/internal/rewrite"
)

var sinkString string

func BenchmarkStripKnownTags(b *testing.B) {
	const tagged = "[GEO:FI][IP:1.2.3.4][SPD:20M] Real Node Name"
	const plain = "Plain Name"

	b.Run("tagged", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sinkString = rewrite.StripKnownTags(tagged)
		}
	})

	b.Run("untagged", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sinkString = rewrite.StripKnownTags(plain)
		}
	})
}

func BenchmarkLeadingTags(b *testing.B) {
	const tagged = "[GEO:FI][IP:1.2.3.4][SPD:20M] Real Node Name"
	const plain = "Plain Name"

	b.Run("tagged", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sinkString = rewrite.LeadingTags(tagged)
		}
	})

	b.Run("untagged", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sinkString = rewrite.LeadingTags(plain)
		}
	})
}
