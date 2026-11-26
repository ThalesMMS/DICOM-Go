// Package surfaceseg reads and writes Surface Segmentation Storage
// instances (PS3.3 A.57): the segments of a segmentation described by
// surfaces and point/line primitives in the Frame of Reference.
//
// A Document mirrors the IOD: Segments (the Segment Description Macro of
// package seg, plus the surfaces each segment is made of, with the
// algorithm that generated them and the instances they were derived from)
// and Surfaces (the Surface Mesh Module: points in patient millimetres,
// optional unit normals, triangles, explicit vertices, edges and directed
// polylines, display colour, opacity, presentation
// type and whether the surface is a closed manifold). Surfaces carry
// mesh.Mesh values, so the meshes of package mesh round-trip: FromMeshes
// builds a document from coloured segment meshes (such as the SurfaceNets
// surfaces of mesh.ExtractLabels), and Document.Parts returns one coloured
// mesh.Part per segment for mesh.WriteGLB, WriteOBJParts or WritePLY. Use
// Document.TriangleParts to reject an export that would lose non-triangle
// primitives; Parts is the historical mesh-only projection.
//
// Write validates the document before encoding it: unique positive segment
// numbers, surface numbers 1 to N in order, every referenced surface
// present, complete coded entries and algorithm identification, finite
// coordinates, every primitive indexing existing points, normals matching the
// points, opacity in [0, 1] and the enumerated values of PS3.3 C.27.1. The
// written instance has the Patient, General Study, General Series,
// Segmentation Series, Frame of Reference, General and Enhanced General
// Equipment, Surface Segmentation, Surface Mesh, Common Instance Reference
// and SOP Common modules (the common ones inherited from a source instance
// through object.DerivedCommon), and package validation/iod finds no
// error in it.
//
// Read accepts what Write produces and what other writers may use: the
// Long and the retired 16-bit point index lists, triangle lists, strips,
// fans and (fan-triangulated) facets. Surface.Primitives preserves explicit
// vertices, edge pairs and Line Sequence paths without triangulation or solid
// semantics. Point indices are 1-based in the data set and 0-based in the model;
// data are read in the object's byte order. Write defaults to Long index lists,
// or writes retired OW lists when RetiredIndexLists is set and there are at most
// 65535 points. Mixed representations normalize to Long; conflicting populated
// Long and retired attributes are rejected. ReadWithLimits bounds aggregate
// counts and geometry bytes before copying numeric arrays; Write enforces the
// same default bounds. Point/line primitives alone imply no area or volume.
//
// Written from PS3.3 (A.57, C.8.23, C.27); no third-party code is copied.
package surfaceseg
