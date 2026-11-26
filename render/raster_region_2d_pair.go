package render

// Layout shared with rasterGraySIMDRow. Each pair packs two covered source
// indices and their Q24 fractions. The admitted source lattice fits uint32.
type rasterGrayColumnPair struct {
	lo, hi [2]uint32
	weight [2]uint64
}

func rasterGrayPairs(columns []rasterGrayAxis) []rasterGrayColumnPair {
	pairs := make([]rasterGrayColumnPair, len(columns)/2)
	for i := range pairs {
		pair := &pairs[i]
		for lane := range 2 {
			column := columns[i*2+lane]
			pair.lo[lane], pair.hi[lane] = uint32(column.lo), uint32(column.hi)
			if column.fraction == 0 {
				pair.hi[lane] = pair.lo[lane]
			}
			pair.weight[lane] = uint64(column.fractionFixed)
		}
	}
	return pairs
}
