package mesh

// The Marching Cubes case table is generated here from first principles
// rather than copied from any implementation (Lorensen and Cline, SIGGRAPH
// 1987, describe the method; the table below is derived, not transcribed).
//
// A cube corner c sits at (c&1, c>>1&1, c>>2&1) in cell coordinates, and a
// case index has bit c set when corner c is inside (sample >= iso). For each
// of the 256 cases the surface is first drawn on the six cube faces: on a
// face, every maximal run of inside corners (walking the face
// counter-clockwise seen from outside the cube) is cut off by one segment
// between the two crossing edges that bound the run. The rule depends only
// on the four corners of the face, so the two cells sharing a face always
// draw the same segments there, which makes the assembled surface
// watertight. On an ambiguous face (two diagonal inside corners) the inside
// corners are cut off separately, the topology of the voxel-face surface
// and of 6-connected inside regions.
//
// The segments are directed (inside on the right, seen from outside), so
// each crossing edge starts exactly one segment and ends exactly one; they
// chain into closed loops on the cube surface. Each loop of n edges is fanned
// into n-2 triangles whose winding is fixed once, by the one-corner case, so
// that triangle normals (right-hand rule in index space) point from the
// inside to the outside.

// cubeEdge is one of the 12 cube edges: the lower corner (bit of axis clear)
// and the axis (0 x, 1 y, 2 z) along which it runs.
type cubeEdge struct {
	base uint8
	axis uint8
}

type caseTable struct {
	edges [12]cubeEdge
	// start[c]..start[c+1] indexes tris for case c; tris holds three edge
	// numbers per triangle.
	start [257]uint16
	tris  []uint8
}

// mcTable is built once at package initialisation; building costs a few
// microseconds.
var mcTable = buildCaseTable()

func (t *caseTable) triangles(c int) []uint8 { return t.tris[t.start[c]:t.start[c+1]] }

func buildCaseTable() *caseTable {
	t := &caseTable{}
	var edgeID [8][8]int
	n := 0
	for a := 0; a < 8; a++ {
		for axis := 0; axis < 3; axis++ {
			if a>>axis&1 == 0 {
				b := a | 1<<axis
				t.edges[n] = cubeEdge{base: uint8(a), axis: uint8(axis)}
				edgeID[a][b], edgeID[b][a] = n, n
				n++
			}
		}
	}
	faces := cubeFaces()
	var edgeFaces [12]uint8
	for f, face := range faces {
		for i := range face {
			edgeFaces[edgeID[face[i]][face[(i+1)%4]]] |= 1 << f
		}
	}
	loopsOf := func(c int) [][]int {
		var next [12]int
		for i := range next {
			next[i] = -1
		}
		for _, face := range faces {
			var in [4]bool
			for i, corner := range face {
				in[i] = c>>corner&1 == 1
			}
			for i := 0; i < 4; i++ {
				prev := (i + 3) % 4
				if !in[i] || in[prev] {
					continue
				}
				j := i
				for in[(j+1)%4] {
					j = (j + 1) % 4
				}
				from := edgeID[face[prev]][face[i]]
				to := edgeID[face[j]][face[(j+1)%4]]
				next[from] = to
			}
		}
		var loops [][]int
		var seen [12]bool
		for e := 0; e < 12; e++ {
			if next[e] < 0 || seen[e] {
				continue
			}
			var loop []int
			for cur := e; !seen[cur]; cur = next[cur] {
				seen[cur] = true
				loop = append(loop, cur)
			}
			loops = append(loops, loop)
		}
		return loops
	}
	// Decide the winding once from case 1 (corner 0 alone inside): the
	// triangle normal must point away from corner 0.
	reverse := false
	{
		loop := loopsOf(1)[0]
		p := func(e int) [3]float64 { return edgeMidpoint(t.edges[e]) }
		a, b, c := p(loop[0]), p(loop[1]), p(loop[2])
		normal := cross3(sub3(b, a), sub3(c, a))
		if dot3(normal, add3(add3(a, b), c)) < 0 { // corner 0 is the origin
			reverse = true
		}
	}
	for c := 0; c < 256; c++ {
		t.start[c] = uint16(len(t.tris))
		for _, loop := range loopsOf(c) {
			if reverse {
				for i, j := 0, len(loop)-1; i < j; i, j = i+1, j-1 {
					loop[i], loop[j] = loop[j], loop[i]
				}
			}
			tris, ok := triangulateLoop(loop, edgeFaces)
			if !ok {
				panic("mesh: no face-safe triangulation for a marching cubes loop")
			}
			for _, tri := range tris {
				t.tris = append(t.tris, uint8(tri[0]), uint8(tri[1]), uint8(tri[2]))
			}
		}
	}
	t.start[256] = uint16(len(t.tris))
	return t
}

// triangulateLoop triangulates a loop of edge numbers without any diagonal
// joining two edges of the same cube face. Such a diagonal would lie in the
// face, where the neighbouring cell can draw the same diagonal (a loop that
// crosses an ambiguous face twice), leaving an edge shared by four
// triangles. Every other diagonal passes through the cell interior and is
// private to it, and the loop's own sides are the face segments shared with
// the neighbour, so each triangle edge is used exactly twice. The search
// prefers the fan from the loop's first edge and otherwise takes the first
// valid split, deterministically.
func triangulateLoop(loop []int, edgeFaces [12]uint8) ([][3]int, bool) {
	n := len(loop)
	chord := func(i, j int) bool {
		if j-i == 1 || (i == 0 && j == n-1) {
			return true // a side of the loop
		}
		return edgeFaces[loop[i]]&edgeFaces[loop[j]] == 0
	}
	memo := map[[2]int]bool{}
	var possible func(i, j int) bool
	possible = func(i, j int) bool {
		if j-i < 2 {
			return true
		}
		key := [2]int{i, j}
		if v, ok := memo[key]; ok {
			return v
		}
		ok := false
		for k := j - 1; k > i && !ok; k-- {
			ok = chord(i, k) && chord(k, j) && possible(i, k) && possible(k, j)
		}
		memo[key] = ok
		return ok
	}
	if !possible(0, n-1) {
		return nil, false
	}
	var tris [][3]int
	var build func(i, j int)
	build = func(i, j int) {
		if j-i < 2 {
			return
		}
		for k := j - 1; k > i; k-- {
			if chord(i, k) && chord(k, j) && possible(i, k) && possible(k, j) {
				tris = append(tris, [3]int{loop[i], loop[k], loop[j]})
				build(i, k)
				build(k, j)
				return
			}
		}
	}
	build(0, n-1)
	return tris, true
}

// cubeFaces lists the four corners of each cube face counter-clockwise seen
// from outside the cube.
func cubeFaces() [6][4]int {
	var faces [6][4]int
	n := 0
	for axis := 0; axis < 3; axis++ {
		u, v := (axis+1)%3, (axis+2)%3
		for side := 0; side < 2; side++ {
			// Counter-clockwise about +axis is (0,0),(1,0),(1,1),(0,1) in
			// (u,v) because u x v = axis; the face at side 0 looks along
			// -axis and runs the other way.
			order := [4][2]int{{0, 0}, {1, 0}, {1, 1}, {0, 1}}
			if side == 0 {
				order = [4][2]int{{0, 0}, {0, 1}, {1, 1}, {1, 0}}
			}
			for i, uv := range order {
				faces[n][i] = side<<axis | uv[0]<<u | uv[1]<<v
			}
			n++
		}
	}
	return faces
}

func edgeMidpoint(e cubeEdge) [3]float64 {
	p := [3]float64{float64(e.base & 1), float64(e.base >> 1 & 1), float64(e.base >> 2 & 1)}
	p[e.axis] += 0.5
	return p
}

func sub3(a, b [3]float64) [3]float64 { return [3]float64{a[0] - b[0], a[1] - b[1], a[2] - b[2]} }
func add3(a, b [3]float64) [3]float64 { return [3]float64{a[0] + b[0], a[1] + b[1], a[2] + b[2]} }
func dot3(a, b [3]float64) float64    { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }
func cross3(a, b [3]float64) [3]float64 {
	return [3]float64{a[1]*b[2] - a[2]*b[1], a[2]*b[0] - a[0]*b[2], a[0]*b[1] - a[1]*b[0]}
}
