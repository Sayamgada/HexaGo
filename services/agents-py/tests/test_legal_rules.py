"""Legal & Jurisdiction: the pure rule extractor (regex fallback), validation and diff."""
import h3
import pytest

from src.agents_py.agents.legal import legal_rules as lr

SAMPLE_NOTICE = ("Karnataka Transport Dept notice: surge pricing for app-based cabs is capped at 1.5x the base fare "
                 "with immediate effect.")
CELL_A = h3.latlng_to_cell(12.9352, 77.6245, 7)   # Koramangala
CELL_B = h3.latlng_to_cell(12.9784, 77.6408, 7)   # Indiranagar


# ---------------------------------------------------------------- the six sample texts (fallback extractor)

@pytest.mark.parametrize("text, expected", [
    (SAMPLE_NOTICE, {"max_surge_multiplier": 1.5}),
    ("Mumbai RTO circular: a surge cap of 1.8x applies to all aggregators.", {"max_surge_multiplier": 1.8}),
    ("Drivers shall work a maximum 10 driving hours in any 24-hour period.", {"max_driving_hours": 10}),
    ("Every cab must carry minimum insurance coverage of Rs 15,00,000 per passenger.",
     {"min_insurance_coverage": 1_500_000}),
    ("The municipal city levy is revised to 3.5% of the fare from 1 December.", {"city_levy_pct": 3.5}),
    ("Delhi notice: surge multiplier shall not exceed 2 times the base fare. Drivers may drive no more than "
     "9 hours a day. Third-party insurance cover of 10 lakh is mandatory.",
     {"max_surge_multiplier": 2.0, "max_driving_hours": 9, "min_insurance_coverage": 1_000_000}),
])
def test_fallback_extractor_on_sample_texts(text, expected):
    got = lr.extract_rules(text)
    assert {k: got[k] for k in expected} == expected
    assert all(got.get(k) is None for k in lr.SCALAR_FIELDS if k not in expected)


def test_extractor_documents_and_modes():
    got = lr.extract_rules("Required documents: driving licence, vehicle registration (RC), PUC certificate "
                           "and police verification.")
    assert got["required_documents"] == ["driving_licence", "vehicle_rc", "puc_certificate", "police_verification"]
    assert got["documents_mode"] == "set"
    add = lr.extract_rules("Drivers must additionally carry a fitness certificate.")
    assert add["required_documents"] == ["fitness_certificate"] and add["documents_mode"] == "add"
    rm = lr.extract_rules("The PUC certificate is no longer required as a document.")
    assert rm["required_documents"] == ["puc_certificate"] and rm["documents_mode"] == "remove"


def test_extractor_av_zones():
    got = lr.extract_rules(f"Autonomous vehicles are permitted only in zones {CELL_A} and {CELL_B}.")
    assert got["av_allowed_zones"] == [CELL_A, CELL_B] and got["av_zones_mode"] == "set"
    add = lr.extract_rules(f"Driverless cabs may now also operate in {CELL_B}.")
    assert add["av_allowed_zones"] == [CELL_B] and add["av_zones_mode"] == "add"
    rm = lr.extract_rules(f"Autonomous vehicles are prohibited in {CELL_A}.")
    assert rm["av_zones_mode"] == "remove"
    ban = lr.extract_rules("Autonomous vehicles are banned citywide until further notice.")
    assert ban["av_allowed_zones"] == [] and ban["av_zones_mode"] == "set"
    bogus = lr.extract_rules("Autonomous vehicles are permitted in 89ffffffffffff1.")
    assert bogus["av_allowed_zones"] is None


def test_extractor_ignores_text_without_rules():
    got = lr.extract_rules("The transport department wishes all citizens a happy Diwali.")
    assert lr.has_changes(got) is False
    assert lr.extract_rules("")["max_surge_multiplier"] is None


def test_extractor_number_forms():
    assert lr.extract_rules("A 2.2x surge ceiling is imposed.")["max_surge_multiplier"] == 2.2
    assert lr.extract_rules("Insurance of ₹2 crore per vehicle.")["min_insurance_coverage"] == 20_000_000
    assert lr.extract_rules("Insurance cover of INR 750000 is required.")["min_insurance_coverage"] == 750_000
    assert lr.extract_rules("Cess of 4 per cent on each trip as city levy.")["city_levy_pct"] == 4.0
    # hours without a driving/work context are not a driving-hours rule
    assert lr.extract_rules("Offices are open a maximum of 8 hours.")["max_driving_hours"] is None


# ---------------------------------------------------------------- cities

def test_city_normalisation_and_inference():
    assert lr.normalize_city("Bangalore") == "bengaluru"
    assert lr.normalize_city(" BOMBAY ") == "mumbai"
    assert lr.normalize_city("New Delhi") == "delhi"
    assert lr.normalize_city("Paris") is None and lr.normalize_city(None) is None
    assert lr.infer_city(SAMPLE_NOTICE) == "bengaluru"
    assert lr.infer_city("Maharashtra notice") == "mumbai"
    assert lr.infer_city("no city here") is None


def test_seed_has_three_cities_with_every_rule():
    seed = lr.seed_jurisdictions()
    assert set(seed) == {"bengaluru", "mumbai", "delhi"}
    for j in seed.values():
        assert set(j["rules"]) == set(lr.RULE_FIELDS)
        assert all(h3.is_valid_cell(c) for c in j["rules"]["av_allowed_zones"])
    assert seed["bengaluru"]["rules"]["max_surge_multiplier"] == 2.5
    assert seed["bengaluru"]["rules"]["city_levy_pct"] == 2.0
    seed["bengaluru"]["rules"]["required_documents"].append("x")
    assert "x" not in lr.seed_jurisdictions()["bengaluru"]["rules"]["required_documents"]  # fresh copy


# ---------------------------------------------------------------- validation, diff, apply

def test_validate_rejects_out_of_bounds_and_coerces():
    valid, rejected = lr.validate_changes({"max_surge_multiplier": 9, "max_driving_hours": "11",
                                           "city_levy_pct": -1, "min_insurance_coverage": 250000.0,
                                           "av_allowed_zones": [CELL_A, "nope"], "av_zones_mode": "add",
                                           "required_documents": ["Driving Licence", "unicorn"]})
    assert valid["max_driving_hours"] == 11 and valid["min_insurance_coverage"] == 250000
    assert valid["av_allowed_zones"] == [CELL_A]
    assert valid["required_documents"] == ["driving_licence", "unicorn"]
    fields = {r["field"] for r in rejected}
    assert fields == {"max_surge_multiplier", "city_levy_pct", "av_allowed_zones"}
    _, rej = lr.validate_changes({"max_driving_hours": "lots"})
    assert rej[0]["field"] == "max_driving_hours"


def test_diff_and_apply():
    old = lr.seed_jurisdictions()["bengaluru"]["rules"]
    changes = {**lr.empty_changes(), "max_surge_multiplier": 1.5, "max_driving_hours": old["max_driving_hours"],
               "required_documents": ["fitness_certificate"], "documents_mode": "add",
               "av_allowed_zones": [old["av_allowed_zones"][0]], "av_zones_mode": "remove"}
    diff = lr.diff_rules(old, changes)
    by = {d["field"]: d for d in diff}
    assert set(by) == {"max_surge_multiplier", "required_documents", "av_allowed_zones"}  # unchanged hours skipped
    assert by["max_surge_multiplier"] == {"field": "max_surge_multiplier", "old": 2.5, "new": 1.5}
    assert by["required_documents"]["new"][-1] == "fitness_certificate"
    assert old["av_allowed_zones"][0] not in by["av_allowed_zones"]["new"]
    new = lr.apply_diff(old, diff)
    assert new["max_surge_multiplier"] == 1.5 and old["max_surge_multiplier"] == 2.5
    # "set" replaces, an equal set is no change
    assert lr.diff_rules(old, {"required_documents": list(old["required_documents"]), "documents_mode": "set"}) == []
    assert lr.diff_rules(old, {"av_allowed_zones": [], "av_zones_mode": "set"})[0]["new"] == []


def test_reconcile_prefers_explicit_numbers_from_the_text():
    llm = {**lr.empty_changes(), "max_surge_multiplier": 1.2, "max_driving_hours": 10}
    rules = {**lr.empty_changes(), "max_surge_multiplier": 1.5}
    merged, overridden = lr.reconcile(llm, rules)
    assert merged["max_surge_multiplier"] == 1.5 and merged["max_driving_hours"] == 10
    assert overridden == ["max_surge_multiplier"]
    merged, overridden = lr.reconcile({**lr.empty_changes()}, rules)
    assert merged["max_surge_multiplier"] == 1.5 and overridden == []


def test_rule_changes_schema_round_trip():
    m = lr.RuleChanges.model_validate({"max_surge_multiplier": 1.5, "summary": "cap"})
    d = m.model_dump()
    assert d["max_surge_multiplier"] == 1.5 and d["documents_mode"] == "set"


def test_describe_diff():
    assert lr.describe_diff([]) == "no rule changes"
    txt = lr.describe_diff([{"field": "max_surge_multiplier", "old": 2.5, "new": 1.5},
                            {"field": "required_documents", "old": ["a"], "new": ["a", "b"]}])
    assert txt == "max_surge_multiplier 2.5 -> 1.5; required_documents +b"
