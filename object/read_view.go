package object

// BorrowReadView returns a facade with independent lookup caches over the
// current object. Keep the original object and all borrowed values unchanged
// while views exist. Use the views for lookups that populate caches. The view
// does not own the original source.
func (o *Object) BorrowReadView() *Object {
	if o == nil {
		return nil
	}
	view := *o
	view.sharedBacking = true
	view.source = nil
	view.invalidateCharacterSetCache()
	return &view
}
