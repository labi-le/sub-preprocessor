package filter_test

import (
	"testing"

	"domains.lst/sub-preprocessor/internal/filter"
	"domains.lst/sub-preprocessor/internal/geofeed"
)

var sinkBool bool

func BenchmarkCountrySetHas(b *testing.B) {
	set := filter.ParseAllowed("DE,US,NL")
	inSet := geofeed.CountryCode{'U', 'S'}
	outSet := geofeed.CountryCode{'G', 'B'}

	b.Run("in_set", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sinkBool = set.Has(inSet)
		}
	})

	b.Run("out_of_set", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sinkBool = set.Has(outSet)
		}
	})
}
