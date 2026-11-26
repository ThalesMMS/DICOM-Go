package colorbar

import "strings"

// petUnits maps the PET Units (0054,1001) defined terms of PS3.3 C.8.9.1.1.3
// to display labels.
var petUnits = map[string]string{
	"CNTS":      "counts",
	"NONE":      "",
	"CM2":       "cm²",
	"CM2ML":     "cm²/ml",
	"PCNT":      "%",
	"CPS":       "counts/s",
	"BQML":      "Bq/ml",
	"MGMINML":   "mg/min/ml",
	"UMOLMINML": "µmol/min/ml",
	"MLMING":    "ml/min/g",
	"MLG":       "ml/g",
	"1CM":       "1/cm",
	"UMOLML":    "µmol/ml",
	"PROPCNTS":  "prop. counts",
	"PROPCPS":   "prop. counts/s",
	"MLMINML":   "ml/min/ml",
	"MLML":      "ml/ml",
	"GML":       "g/ml",
	"STDDEV":    "SD",
}

// PETUnitsLabel returns the display unit for a PET Units (0054,1001) value.
// Unknown and unitless ("NONE") values return "", so callers never present an
// invented unit for a quantitative legend.
func PETUnitsLabel(code string) string {
	return petUnits[strings.ToUpper(strings.TrimSpace(code))]
}
