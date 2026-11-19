import 'dart:convert';

import 'package:dio/dio.dart';

/// Legal & Jurisdiction agent: city rules, regulation ingest, audit log
/// (agents-py through the gateway's `/v1/legal/*` proxy).

const defaultSampleNotice = 'Karnataka Transport Dept notice: surge pricing for app-based cabs is capped at 1.5x the base '
    'fare with immediate effect.';

Map<String, dynamic> _map(Object? v) => v is Map ? Map<String, dynamic>.from(v) : <String, dynamic>{};
List<String> _strings(Object? v) => [if (v is List) for (final x in v) '$x'];
double _num(Object? v, [double d = 0]) => v is num ? v.toDouble() : (double.tryParse('$v') ?? d);

class LegalRules {
  const LegalRules({
    required this.maxSurge,
    required this.minInsurance,
    required this.maxDrivingHours,
    required this.avZones,
    required this.documents,
    required this.cityLevyPct,
  });

  final double maxSurge;
  final int minInsurance;
  final int maxDrivingHours;
  final List<String> avZones;
  final List<String> documents;
  final double cityLevyPct;

  factory LegalRules.fromJson(Map<String, dynamic> j) => LegalRules(
        maxSurge: _num(j['max_surge_multiplier'], 1),
        minInsurance: _num(j['min_insurance_coverage']).round(),
        maxDrivingHours: _num(j['max_driving_hours']).round(),
        avZones: _strings(j['av_allowed_zones']),
        documents: _strings(j['required_documents']),
        cityLevyPct: _num(j['city_levy_pct']),
      );

  Map<String, dynamic> toJson() => {
        'max_surge_multiplier': maxSurge,
        'min_insurance_coverage': minInsurance,
        'max_driving_hours': maxDrivingHours,
        'av_allowed_zones': avZones,
        'required_documents': documents,
        'city_levy_pct': cityLevyPct,
      };

  LegalRules copyWith({double? maxSurge}) => LegalRules(
        maxSurge: maxSurge ?? this.maxSurge,
        minInsurance: minInsurance,
        maxDrivingHours: maxDrivingHours,
        avZones: avZones,
        documents: documents,
        cityLevyPct: cityLevyPct,
      );
}

class Jurisdiction {
  const Jurisdiction({
    required this.city,
    required this.name,
    required this.rules,
    this.state = '',
    this.authority = '',
    this.version = 1,
    this.updatedAt,
    this.updatedBy = '',
  });

  final String city;
  final String name;
  final String state;
  final String authority;
  final LegalRules rules;
  final int version;
  final DateTime? updatedAt;
  final String updatedBy;

  factory Jurisdiction.fromJson(Map<String, dynamic> j) => Jurisdiction(
        city: '${j['city'] ?? ''}',
        name: '${j['name'] ?? j['city'] ?? ''}',
        state: '${j['state'] ?? ''}',
        authority: '${j['authority'] ?? ''}',
        rules: LegalRules.fromJson(_map(j['rules'])),
        version: _num(j['version'], 1).round(),
        updatedAt: DateTime.tryParse('${j['updated_at'] ?? ''}'),
        updatedBy: '${j['updated_by'] ?? ''}',
      );
}

class JurisdictionList {
  const JurisdictionList({
    required this.jurisdictions,
    this.enabled = true,
    this.pricingCity = 'bengaluru',
    this.sampleNotice = defaultSampleNotice,
  });

  final List<Jurisdiction> jurisdictions;
  final bool enabled;
  final String pricingCity;
  final String sampleNotice;

  factory JurisdictionList.fromJson(Map<String, dynamic> j) => JurisdictionList(
        enabled: j['enabled'] != false,
        pricingCity: '${j['pricing_city'] ?? 'bengaluru'}',
        sampleNotice: (j['sample_notice'] as String?) ?? defaultSampleNotice,
        jurisdictions: [
          if (j['jurisdictions'] is List)
            for (final x in j['jurisdictions'] as List)
              if (x is Map) Jurisdiction.fromJson(Map<String, dynamic>.from(x)),
        ],
      );

  Jurisdiction? byCity(String city) {
    for (final j in jurisdictions) {
      if (j.city == city) return j;
    }
    return null;
  }
}

class RuleDiff {
  const RuleDiff({required this.field, this.oldValue, this.newValue});

  final String field;
  final Object? oldValue;
  final Object? newValue;

  factory RuleDiff.fromJson(Map<String, dynamic> j) =>
      RuleDiff(field: '${j['field'] ?? ''}', oldValue: j['old'], newValue: j['new']);
}

class IngestResult {
  const IngestResult({
    required this.city,
    required this.jurisdiction,
    required this.diff,
    required this.applied,
    this.rejected = const [],
    this.overridden = const [],
    this.auditId = '',
    this.source = 'rules',
    this.model = '',
    this.usedFallback = false,
    this.fallbackReason,
    this.summary = '',
  });

  final String city;
  final Jurisdiction jurisdiction;
  final List<RuleDiff> diff;
  final bool applied;
  final List<Map<String, dynamic>> rejected;
  final List<String> overridden;
  final String auditId;
  final String source;
  final String model;
  final bool usedFallback;
  final String? fallbackReason;
  final String summary;

  factory IngestResult.fromJson(Map<String, dynamic> j) => IngestResult(
        city: '${j['city'] ?? ''}',
        jurisdiction: Jurisdiction.fromJson(_map(j['jurisdiction'])),
        diff: [
          if (j['diff'] is List)
            for (final d in j['diff'] as List)
              if (d is Map) RuleDiff.fromJson(Map<String, dynamic>.from(d)),
        ],
        applied: j['applied'] == true,
        rejected: [
          if (j['rejected'] is List)
            for (final r in j['rejected'] as List)
              if (r is Map) Map<String, dynamic>.from(r),
        ],
        overridden: _strings(j['overridden']),
        auditId: '${j['audit_id'] ?? ''}',
        source: '${j['source'] ?? 'rules'}',
        model: '${j['model'] ?? ''}',
        usedFallback: j['used_fallback'] == true,
        fallbackReason: j['fallback_reason'] as String?,
        summary: '${j['summary'] ?? ''}',
      );
}

class AuditEntry {
  const AuditEntry({
    required this.id,
    required this.city,
    required this.diff,
    this.createdAt,
    this.source = '',
    this.summary = '',
  });

  final String id;
  final String city;
  final List<RuleDiff> diff;
  final DateTime? createdAt;
  final String source;
  final String summary;

  factory AuditEntry.fromJson(Map<String, dynamic> j) => AuditEntry(
        id: '${j['id'] ?? ''}',
        city: '${j['city'] ?? ''}',
        createdAt: DateTime.tryParse('${j['created_at'] ?? ''}'),
        source: '${j['source'] ?? ''}',
        summary: '${j['summary'] ?? ''}',
        diff: [
          if (j['diff'] is List)
            for (final d in j['diff'] as List)
              if (d is Map) RuleDiff.fromJson(Map<String, dynamic>.from(d)),
        ],
      );
}

/// Display label of a rule field.
String ruleLabel(String field) => switch (field) {
      'max_surge_multiplier' => 'Surge cap',
      'min_insurance_coverage' => 'Min insurance',
      'max_driving_hours' => 'Max driving hours',
      'av_allowed_zones' => 'AV zones',
      'required_documents' => 'Required documents',
      'city_levy_pct' => 'City levy',
      _ => field,
    };

/// Indian digit grouping: 1500000 -> ₹15,00,000.
String formatInr(int rupees) {
  final s = rupees.abs().toString();
  String grouped;
  if (s.length <= 3) {
    grouped = s;
  } else {
    final head = s.substring(0, s.length - 3);
    final tail = s.substring(s.length - 3);
    final parts = <String>[];
    for (var i = head.length; i > 0; i -= 2) {
      parts.insert(0, head.substring(i - 2 < 0 ? 0 : i - 2, i));
    }
    grouped = '${parts.join(',')},$tail';
  }
  return '${rupees < 0 ? '-' : ''}₹$grouped';
}

String _trim(double v) => v == v.roundToDouble() ? v.toStringAsFixed(1) : '$v';

/// A rule value for display: '1.5x', '₹5,00,000', '12 h', '2.0%', or a list count.
String formatRule(String field, Object? v) {
  if (v == null) return '—';
  if (v is List) return v.isEmpty ? 'none' : v.join(', ');
  final n = _num(v);
  return switch (field) {
    'max_surge_multiplier' => '${_trim(n)}x',
    'min_insurance_coverage' => formatInr(n.round()),
    'max_driving_hours' => '${n.round()} h',
    'city_levy_pct' => '${_trim(n)}%',
    _ => '$v',
  };
}

abstract class LegalApi {
  Future<JurisdictionList> jurisdictions();
  Future<IngestResult> ingest(String text, {String? city});
  Future<List<AuditEntry>> audit({int limit = 10});
}

Map<String, dynamic> _json(Object? data) => _map(data is String ? jsonDecode(data) : data);

class DioLegalApi implements LegalApi {
  DioLegalApi({required String apiBaseUrl, Dio? dio})
      : _api = apiBaseUrl.endsWith('/') ? apiBaseUrl.substring(0, apiBaseUrl.length - 1) : apiBaseUrl,
        _dio = dio ??
            Dio(BaseOptions(connectTimeout: const Duration(seconds: 60), receiveTimeout: const Duration(seconds: 60)));

  final String _api;
  final Dio _dio;

  @override
  Future<JurisdictionList> jurisdictions() async =>
      JurisdictionList.fromJson(_json((await _dio.get<dynamic>('$_api/v1/legal/jurisdictions')).data));

  @override
  Future<IngestResult> ingest(String text, {String? city}) async => IngestResult.fromJson(_json((await _dio
          .post<dynamic>('$_api/v1/legal/ingest', data: {'text': text, 'city': ?city}))
      .data));

  @override
  Future<List<AuditEntry>> audit({int limit = 10}) async {
    final list = _json((await _dio.get<dynamic>('$_api/v1/legal/audit', queryParameters: {'limit': limit})).data)['entries'];
    return [
      if (list is List)
        for (final e in list)
          if (e is Map) AuditEntry.fromJson(Map<String, dynamic>.from(e)),
    ];
  }
}

/// Offline demo (USE_MOCK, tests): three seeded cities; ingest understands `<n>x` surge caps.
class MockLegalApi implements LegalApi {
  MockLegalApi() {
    const docs = ['driving_licence', 'vehicle_rc', 'insurance_certificate', 'puc_certificate', 'police_verification'];
    for (final (city, name, cap, zones) in [
      ('bengaluru', 'Bengaluru', 2.5, ['876189260ffffff', '87618921dffffff']),
      ('delhi', 'Delhi', 2.0, <String>[]),
      ('mumbai', 'Mumbai', 2.0, ['87608b0b6ffffff']),
    ]) {
      _cities[city] = Jurisdiction(
        city: city,
        name: name,
        rules: LegalRules(
            maxSurge: cap, minInsurance: 500000, maxDrivingHours: 12, avZones: zones, documents: docs, cityLevyPct: 2),
      );
    }
  }

  final Map<String, Jurisdiction> _cities = {};
  final List<AuditEntry> _audit = [];

  @override
  Future<JurisdictionList> jurisdictions() async =>
      JurisdictionList(jurisdictions: [for (final c in _cities.keys.toList()..sort()) _cities[c]!]);

  @override
  Future<IngestResult> ingest(String text, {String? city}) async {
    final low = text.toLowerCase();
    final key = city ??
        (low.contains('maharashtra') || low.contains('mumbai')
            ? 'mumbai'
            : low.contains('delhi')
                ? 'delhi'
                : 'bengaluru');
    final cur = _cities[key]!;
    final m = RegExp(r'(\d+(?:\.\d+)?)\s*x\b').firstMatch(low);
    final cap = low.contains('surge') && m != null ? double.parse(m.group(1)!) : null;
    final diff = <RuleDiff>[
      if (cap != null && cap != cur.rules.maxSurge)
        RuleDiff(field: 'max_surge_multiplier', oldValue: cur.rules.maxSurge, newValue: cap),
    ];
    var j = cur;
    if (diff.isNotEmpty) {
      j = Jurisdiction(
          city: cur.city, name: cur.name, rules: cur.rules.copyWith(maxSurge: cap), version: cur.version + 1,
          updatedAt: DateTime.now().toUtc(), updatedBy: 'legal_jurisdiction');
      _cities[key] = j;
    }
    final id = 'lga_mock_${_audit.length + 1}';
    final summary = diff.isEmpty ? 'No rule changes found in the text.' : 'Surge capped at ${_trim(cap!)}x.';
    _audit.insert(0, AuditEntry(id: id, city: key, diff: diff, createdAt: DateTime.now().toUtc(), source: 'rules', summary: summary));
    return IngestResult(
        city: key, jurisdiction: j, diff: diff, applied: diff.isNotEmpty, auditId: id, source: 'rules',
        model: 'rule_extractor', usedFallback: true, fallbackReason: 'disabled', summary: summary);
  }

  @override
  Future<List<AuditEntry>> audit({int limit = 10}) async => _audit.take(limit).toList();
}
