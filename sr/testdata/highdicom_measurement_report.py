"""Summarize a TID 1500 Measurement Report as highdicom reads it.

Used by TestMeasurementReportReadsInHighdicom (measurement_report_highdicom_test.go):
the report is opened with highdicom.sr.srread, its content is parsed with
highdicom's MeasurementReport and the groups, tracking identifiers,
measurements and references highdicom finds are printed as JSON for the Go
test to compare with the report it wrote. Nothing here decides pass or fail
beyond highdicom accepting the report.
"""

import json
import sys

import highdicom as hd
import pydicom
from pydicom.sr.codedict import codes


def code(value):
    if value is None:
        return None
    return {"value": value.value, "scheme": value.scheme_designator, "meaning": value.meaning}


def image(item):
    ref = item.ReferencedSOPSequence[0]
    return {"sop_class": str(ref.ReferencedSOPClassUID), "sop_instance": str(ref.ReferencedSOPInstanceUID)}


def measurement(m):
    out = {
        "name": code(m.name),
        "value": float(m.value),
        "unit": code(m.unit),
        "derivation": code(m.derivation),
        "images": [image(i) for i in m.referenced_images],
        "coordinates": [],
    }
    for c in m.referenced_coordinates:
        out["coordinates"].append({
            "graphic_type": c.graphic_type.value,
            "frame_of_reference": str(c.frame_of_reference_uid),
            "data": [float(v) for v in c.value.flatten()],
        })
    return out


def group(g, kind):
    out = {
        "kind": kind,
        "tracking_identifier": g.tracking_identifier,
        "tracking_uid": str(g.tracking_uid) if g.tracking_uid else None,
        "finding": code(g.finding_type),
        "finding_sites": [code(s.value) for s in g.finding_sites],
        "measurements": [measurement(m) for m in g.get_measurements()],
    }
    if kind == "volumetric":
        segment = g.referenced_segment
        source = segment.source_series_for_segmentation
        out["segment"] = {
            "sop_class": str(segment.referenced_sop_class_uid),
            "sop_instance": str(segment.referenced_sop_instance_uid),
            "segment_numbers": [int(n) for n in segment.referenced_segment_numbers],
            "source_series": str(source.value) if source is not None else None,
        }
    return out


def main(path):
    document = hd.sr.srread(path)
    report = document.content
    if not isinstance(report, hd.sr.MeasurementReport):
        sys.exit(f"highdicom did not read a MeasurementReport: {type(report).__name__}")
    observers = []
    for context in report.get_observer_contexts():
        attributes = context.observer_identifying_attributes
        entry = {"type": code(context.observer_type)}
        if isinstance(attributes, hd.sr.PersonObserverIdentifyingAttributes):
            entry["person_name"] = str(attributes.name)
        else:
            entry["device_uid"] = str(attributes.uid)
            entry["device_name"] = attributes.name
        observers.append(entry)
    library = []
    for container in report[0].ContentSequence:
        concept = hd.sr.CodedConcept.from_dataset(container.ConceptNameCodeSequence[0])
        if concept != codes.DCM.ImageLibrary:
            continue
        for item in hd.sr.utils.find_content_items(container, value_type=hd.sr.ValueTypeValues.IMAGE, recursive=True):
            library.append(image(item))
    summary = {
        "versions": {"highdicom": hd.__version__, "pydicom": pydicom.__version__},
        "sop_class": str(document.SOPClassUID),
        "template": str(report[0].ContentTemplateSequence[0].TemplateIdentifier),
        "observers": observers,
        "image_library": library,
        "groups": [group(g, "image") for g in report.get_image_measurement_groups()]
        + [group(g, "volumetric") for g in report.get_volumetric_roi_measurement_groups()],
        "evidence": [
            {"study": str(st), "series": str(se), "sop_instance": str(inst), "sop_class": str(cls)}
            for st, se, inst, cls in document.get_evidence()
        ],
    }
    json.dump(summary, sys.stdout, indent=1)


if __name__ == "__main__":
    main(sys.argv[1])
