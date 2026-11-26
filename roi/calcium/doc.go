// Package calcium scores coronary artery calcium on non-contrast CT: the
// Agatston score, the volume score and the calcium mass score, per coronary
// artery (left main, left anterior descending, left circumflex, right
// coronary and other coronary branches).
//
// # Method
//
// Detect reads the slices of a CT stack in Hounsfield units and finds the
// candidate lesions of each slice: the 8-connected (or 4-connected) regions
// of pixels at or above the threshold (130 HU) whose area is at least the
// minimum lesion area (1 mm²). Smaller regions are noise and are not lesions.
// The candidates are not yet calcium of a coronary artery: bone, valve and
// aortic calcium pass the same test. A reader assigns lesions to a vessel
// (Assign, or AssignGroup for every slice of a lesion connected through
// adjacent slices), and only assigned lesions count in the per-vessel and
// total scores; the rest stay listed as unassigned candidates.
//
// For each lesion of a slice:
//
//   - Agatston score = area (mm²) × density weight, where the weight comes from
//     the lesion's peak attenuation: 1 for 130–199 HU, 2 for 200–299 HU,
//     3 for 300–399 HU and 4 for 400 HU or more (Agatston et al. 1990). With a
//     lower threshold, peaks below 200 HU still weigh 1.
//   - Volume score = area (mm²) × slice increment (mm), the volume of the voxels
//     above the threshold (Callister et al. 1998, without their isotropic
//     interpolation).
//   - Mass score = c × volume (cm³) × mean attenuation (HU), in milligrams of
//     calcium hydroxyapatite, where c (mg/cm³ per HU) is the calibration
//     factor measured with a calcium hydroxyapatite phantom on the scanner
//     (Hong et al. 2002; Ulzheimer and Kalender 2003). Without a calibration
//     factor no mass is reported.
//
// # Slice thickness and increment
//
// Agatston defined the score on contiguous 3 mm slices. Each slice's score
// stands for the slab it represents, so the summed Agatston score is
// multiplied by increment / 3 mm (NormalizationFactor): contiguous 3 mm slices
// are unchanged, while thinner or overlapping reconstructions count each
// millimetre of the heart once. The increment is the distance between
// adjacent slice positions; the stack must be evenly spaced (a missing slice
// or a duplicate position is refused) and, for a single slice, the slice
// thickness is used. Scores of reconstructions other than 3 mm without
// overlap are not interchangeable with the original definition even after
// normalization; the slice thickness is reported for that reason.
//
// The method assumes a non-contrast, ECG-triggered CT acquired for calcium
// scoring (120 kVp for the 130 HU threshold). CheckSource refuses non-CT
// images and images that record an administered contrast agent.
//
// # Provenance
//
// Implemented from the publications, not from Horos, 3D Slicer or ITK code:
//
//   - Agatston AS, Janowitz WR, Hildner FJ, et al. Quantification of coronary
//     artery calcium using ultrafast computed tomography. J Am Coll Cardiol.
//     1990;15(4):827–832.
//   - Callister TQ, Cooil B, Raya SP, et al. Coronary artery disease: improved
//     reproducibility of calcium scoring with an electron-beam CT volumetric
//     method. Radiology. 1998;208(3):807–814.
//   - Hong C, Becker CR, Schoepf UJ, et al. Coronary artery calcium: absolute
//     quantification in nonenhanced and contrast-enhanced multi-detector row
//     CT studies. Radiology. 2002;223(2):474–480.
//   - Ulzheimer S, Kalender WA. Assessment of calcium scoring performance in
//     cardiac computed tomography. Eur Radiol. 2003;13(3):484–497.
//
// MeasurementReport writes the scores as a TID 1500 Measurement Report with
// one measurement group per vessel and one for the total, using the DICOM
// calcium scoring concepts (Calcium Volume, Calcium Mass, Agatston Score
// Threshold, Calcium Scoring Calibration) and SNOMED CT "Coronary artery
// calcium score".
package calcium
