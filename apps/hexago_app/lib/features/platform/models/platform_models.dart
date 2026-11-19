/// Wire models for agents-py's platform endpoints (through the gateway):
/// `GET /v1/agents` and `GET /v1/pricing/surge-map`.
library;

double _d(Object? v, [double f = 0]) => v is num ? v.toDouble() : (v is String ? double.tryParse(v) ?? f : f);
int _i(Object? v) => v is int ? v : (v is num ? v.round() : (v is String ? int.tryParse(v) ?? 0 : 0));
String _s(Object? v) => v?.toString() ?? '';
Map<String, dynamic> _m(Object? v) => v is Map ? Map<String, dynamic>.from(v) : const {};

/// One of the 11 roster agents with its last-hour activity.
class AgentInfo {
  const AgentInfo({
    required this.id,
    required this.name,
    required this.status,
    this.responsibility = '',
    this.fallbackMode = '',
    this.runsIn = '',
    this.decisionsLastHour = 0,
    this.fallbackRate = 0,
    this.lastDecision = '',
    this.lastReasoning = '',
    this.lastAt,
    this.lastModel = '',
  });

  final String id;
  final String name;

  /// `active` or `planned`.
  final String status;
  final String responsibility;
  final String fallbackMode;
  final String runsIn;
  final int decisionsLastHour;

  /// 0..1
  final double fallbackRate;
  final String lastDecision;
  final String lastReasoning;
  final DateTime? lastAt;

  /// The model behind the agent's latest model-backed decision (empty: rules only).
  final String lastModel;

  bool get active => status == 'active';

  factory AgentInfo.fromJson(Map<String, dynamic> j) {
    final last = _m(j['last_decision']);
    return AgentInfo(
      id: _s(j['id']),
      name: _s(j['name']).isEmpty ? _s(j['id']) : _s(j['name']),
      status: _s(j['status']).isEmpty ? 'planned' : _s(j['status']),
      responsibility: _s(j['responsibility']),
      fallbackMode: _s(j['fallback_mode']),
      runsIn: _s(j['runs_in']),
      decisionsLastHour: _i(j['decisions_last_hour']),
      fallbackRate: _d(j['fallback_rate']).clamp(0, 1).toDouble(),
      lastDecision: _s(last['decision']),
      lastReasoning: _s(last['reasoning']),
      lastAt: DateTime.tryParse(_s(last['ts'])),
      lastModel: _s(j['last_model']),
    );
  }

  static List<AgentInfo> listFromJson(Object? body) {
    final list = _m(body)['agents'];
    if (list is! List) return const [];
    return [
      for (final a in list)
        if (a is Map) AgentInfo.fromJson(Map<String, dynamic>.from(a)),
    ];
  }
}

/// One priced H3 cell of the surge map.
class SurgeCell {
  const SurgeCell({
    required this.cell,
    required this.multiplier,
    required this.boundary,
    required this.centerLat,
    required this.centerLng,
    this.demand = 0,
    this.supply = 0,
    this.explanation = '',
  });

  final String cell;
  final double multiplier;

  /// Polygon vertices as (lat, lng).
  final List<(double, double)> boundary;
  final double centerLat;
  final double centerLng;
  final int demand;
  final int supply;
  final String explanation;

  /// "1.4x"
  String get label => '${multiplier.toStringAsFixed(1)}x';

  bool get surging => multiplier > 1.0;

  factory SurgeCell.fromJson(Map<String, dynamic> j) {
    final c = _m(j['center']);
    final pts = <(double, double)>[];
    final b = j['boundary'];
    if (b is List) {
      for (final p in b) {
        if (p is List && p.length >= 2) pts.add((_d(p[0]), _d(p[1])));
      }
    }
    return SurgeCell(
      cell: _s(j['cell']),
      multiplier: _d(j['multiplier'], 1),
      boundary: pts,
      centerLat: _d(c['lat']),
      centerLng: _d(c['lng']),
      demand: _i(j['demand']),
      supply: _i(j['supply']),
      explanation: _s(j['explanation']),
    );
  }
}

class SurgeMapData {
  const SurgeMapData({this.cells = const [], this.asOf, this.source = ''});

  final List<SurgeCell> cells;
  final DateTime? asOf;
  final String source;

  /// Cells above 1.0x (the ones drawn), highest first.
  List<SurgeCell> get surging => [
    for (final c in cells)
      if (c.surging && c.boundary.length >= 3) c,
  ]..sort((a, b) => b.multiplier.compareTo(a.multiplier));

  factory SurgeMapData.fromJson(Map<String, dynamic> j) {
    final list = j['cells'];
    return SurgeMapData(
      cells: [
        if (list is List)
          for (final c in list)
            if (c is Map) SurgeCell.fromJson(Map<String, dynamic>.from(c)),
      ],
      asOf: DateTime.tryParse(_s(j['as_of'])),
      source: _s(j['source']),
    );
  }
}
