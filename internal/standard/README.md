# Standard Dictionary Source

`internal/standard/dicom.dic` and `internal/standard/uids.tsv` are the versioned
input sources for `cmd/dicomdictgen` and `cmd/dicomuidgen`;
`internal/standard/sr_relationships.tsv` is the input of `cmd/srrelgen` and
`internal/standard/sr_templates.tsv` the input of `cmd/srtemplategen`.

Source:


UID source:

- Table: `DICOM PS 3.6` table `A-1`
- Vendored file: `internal/standard/uids.tsv`
- Derived from the DICOM PS3.6 table A-1 standard source on April 23, 2026
- Compared with PS3.6 2026e table A-1 (`part06.xml`) on November 25, 2026: the
  three CT For Processing Storage SOP Classes of Supplement 252
  (`1.2.840.10008.5.1.4.1.1.2.3` to `.2.5`) were added. The other differences
  were not taken: two retired UIDs without a name or keyword
  (`1.2.840.10008.5.1.4.1.1.12.77` and `.40`), and the UID Types "Storage SOP
  Class" and "Image Storage SOP Class" that 2026e (CP-2588) uses instead of
  "SOP Class", which the TSV keeps as `SOP Class`; the image classification is
  the category of the Storage SOP Class catalog below

Storage SOP Class catalog source (`storage_sop_classes.tsv`):


SR relationship content constraints source:

- Tables: `DICOM PS3.3` Tables A.35.1-2 to A.35.23-2 ("Relationship Content
  Constraints") and the "Relationship Constraints" text of each SR Document IOD
  (by-value only, by-reference allowed, or allowed for listed relationships)
- Edition and extraction date: recorded in the TSV header (currently 2026d)
- Extracted by `go run ./cmd/srrelgen -extract <dir> -date YYYY-MM-DD` from the
  PS3.3 chtml section pages `sect_A.35*.html` of that edition, saved from
  `https://dicom.nema.org/medical/dicom/current/output/chtml/part03/`; the
  extractor refuses unknown value types, relationships, IOD names or policy
  wording instead of guessing

PS3.16 terminology source (`codes/terms.tsv` and `codes/cid.tsv`, next to the
`codes` package that embeds them):

- Content: the DICOM Controlled Terminology of Annex D (Table D-1, retired
  codes flagged), every Context Group of Annex B (keyword, extensibility,
  version, UID, members and Include rows), and the codes of other schemes
  that the Context Groups and templates use (`EV`/`DT` triplets), each with
  the meaning of its first Context Group, else its most frequent template
  meaning
- Edition and SHA-256 of the source: recorded in both TSV headers (currently
  PS3.16 2026d)
- Extracted by `go run ./cmd/terminologygen -extract part16.xml -dir codes`
  from the DocBook saved from
  `https://dicom.nema.org/medical/dicom/current/source/docbook/part16/part16.xml`;
  `go generate ./codes` then writes the named constants of `codes/dcm`,
  `codes/sct`, `codes/ucum`, `codes/umls` and `codes/ln`

PS3.16 SR template catalog source (`sr_templates.tsv`):


Why these files are committed:

- dictionary generation stays reproducible without network access
- entry ordering in `dictionary/std/std_gen.go` is deterministic
- UID entry ordering in `dictionary/uid/std_gen.go` is deterministic
- updates are explicit reviewable changes instead of implicit fetches at build time

Update procedure:

1. Copy `dcmdata/data/dicom.dic` of the chosen DCMTK commit into
   `internal/standard/dicom.dic` (`git -C <dcmtk> show <commit>:dcmdata/data/dicom.dic`).
2. Review upstream header changes, especially the PS3.6/PS3.7 version lines.
3. Update `-commit` in `dictionary/std/doc.go`, run `go generate ./dictionary/std`,
   and update the provenance entry (hashes, line range, commit) and the notice.
4. Update `internal/standard/uids.tsv` from the current PS3.6 table A-1 source.
5. Run `go generate ./dictionary/uid`.
6. Re-run `go generate ./dictionary/std` and `go generate ./dictionary/uid`, then confirm the outputs are byte-identical.
7. Save the PS3.3 `sect_A.35*.html` pages of the new edition, run
   `go run ./cmd/srrelgen -extract <dir> -date <today>`, review the TSV diff, then
   run `go generate ./sr`.
8. Save the PS3.4 `sect_B.5.html`, `sect_B.6.html` and `sect_GG.3.html` pages
   of the new edition, run
   `go run ./cmd/storageclassgen -extract <dir> -date <today>` (after step 4, so
   the PS3.6 keywords and the classes PS3.4 does not list are current), review
   the TSV diff (a new class name without a rule stops the extraction), then
   run `go generate ./dictionary/uid`.
9. Save the new `part16.xml`, run
   `go run ./cmd/terminologygen -extract part16.xml -dir codes`, and review the
   TSV and generated diffs: renamed constants follow changed Code Meanings.
10. With the same `part16.xml`, run
   `go run ./cmd/srtemplategen -extract part16.xml -date <today>`, review the
   TSV diff (the curated subset becomes the full catalog; update its
   `THIRD_PARTY_PROVENANCE.json` entry, which then no longer depends on
   DICOM-Swift), then run `go generate ./sr`.
11. Run `go test ./...` (with `DICOMGO_DCMTK_INTEGRATION=1` to compare the SR
   tables with DCMTK).

Implementation notes for the Go generator:

- Repeating tag ranges are expanded to concrete tags because `dicom-go` currently exposes exact-tag lookup only.
- Context-dependent VRs are relaxed to exact Go VRs: `xs -> US`, `ox -> OW`, `px -> OW`, `lt -> OW`, `up -> UL`.
- Human-readable `Entry.Name` values are derived from the DICOM keyword because `dicom.dic` does not carry the long display name field used by `dicom-go`.
- The UID source is committed as TSV instead of fetching the PS3.6 DocBook XML during generation.
- Keyword lookup in the Go UID registry is case-insensitive to match the existing `dictionary` package contract.
