// Package qrmatch implements DICOM PS3.4 C.2.2 Query/Retrieve matching without
// depending on an archive or database. Callers supply query and candidate
// values; the matcher applies universal, single-value, UID-list, wildcard,
// range and same-item sequence rules under explicit resource limits.
// Raw text governed by Specific Character Set is decoded using its own
// dataset or sequence-item scope. An unreadable supported query key is
// invalid. An unreadable candidate key fails that comparison.
package qrmatch
