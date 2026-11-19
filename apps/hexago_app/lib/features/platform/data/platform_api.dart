import 'dart:convert';
import 'dart:math' as math;

import 'package:dio/dio.dart';

import '../../ops/models/ops_models.dart';
import '../models/platform_models.dart';

/// Agent roster, recent agent events, the surge map and wake-up probes.
abstract class PlatformApi {
  /// `GET /v1/agents`.
  Future<List<AgentInfo>> agents();

  /// `GET /v1/agents/events?limit=&per_agent=`: recent events, newest first,
  /// at most [perAgent] per agent so every agent shows up.
  Future<List<AgentEvent>> recentEvents({int limit = 80, int perAgent = 8});

  /// `GET /v1/pricing/surge-map`.
  Future<SurgeMapData> surgeMap();

  /// `GET /healthz` (and `GET /v1/agents` when [agents]); throws while asleep.
  Future<void> probe({bool agents = false});
}

Map<String, dynamic> _json(Object? data) {
  final d = data is String ? jsonDecode(data) : data;
  return d is Map ? Map<String, dynamic>.from(d) : <String, dynamic>{};
}

class DioPlatformApi implements PlatformApi {
  DioPlatformApi({required String apiBaseUrl, Dio? dio})
    : _api = apiBaseUrl.endsWith('/') ? apiBaseUrl.substring(0, apiBaseUrl.length - 1) : apiBaseUrl,
      _dio =
          dio ??
          Dio(BaseOptions(connectTimeout: const Duration(seconds: 20), receiveTimeout: const Duration(seconds: 20)));

  final String _api;
  final Dio _dio;

  /// Probes give up quickly; the waking screen retries every 5 s.
  static final _probe = Options(sendTimeout: const Duration(seconds: 8), receiveTimeout: const Duration(seconds: 8));

  @override
  Future<List<AgentInfo>> agents() async =>
      AgentInfo.listFromJson(_json((await _dio.get<dynamic>('$_api/v1/agents')).data));

  @override
  Future<List<AgentEvent>> recentEvents({int limit = 80, int perAgent = 8}) async {
    final res = await _dio.get<dynamic>(
      '$_api/v1/agents/events',
      queryParameters: {'limit': limit, 'per_agent': perAgent},
    );
    final list = _json(res.data)['events'];
    return [
      if (list is List)
        for (final e in list)
          if (e is Map) AgentEvent.fromJson(Map<String, dynamic>.from(e)),
    ];
  }

  @override
  Future<SurgeMapData> surgeMap() async =>
      SurgeMapData.fromJson(_json((await _dio.get<dynamic>('$_api/v1/pricing/surge-map')).data));

  @override
  Future<void> probe({bool agents = false}) async {
    await _dio.get<dynamic>('$_api/healthz', options: _probe);
    if (agents) await _dio.get<dynamic>('$_api/v1/agents', options: _probe);
  }
}

/// Offline demo data (USE_MOCK, tests).
class MockPlatformApi implements PlatformApi {
  MockPlatformApi({this.centerLat = 12.9716, this.centerLng = 77.5946});

  final double centerLat;
  final double centerLng;

  static const roster = <(String, String, bool)>[
    ('booking_intent', 'Booking Intent', true),
    ('dispatch_broker', 'Dispatch Broker', true),
    ('pool_batching', 'Pool Batching', false),
    ('surge_equilibrium', 'Surge Equilibrium', true),
    ('demand_forecast', 'Demand Forecast', false),
    ('safety_sentinel', 'Safety Sentinel', true),
    ('compliance_vision', 'Compliance Vision', false),
    ('legal_jurisdiction', 'Legal & Jurisdiction', false),
    ('settlement', 'Settlement', true),
    ('support_dispute', 'Support & Dispute', false),
    ('ops_supervisor', 'Ops Supervisor', true),
  ];

  @override
  Future<List<AgentInfo>> agents() async {
    final now = DateTime.now().toUtc();
    var i = 0;
    return [
      for (final (id, name, active) in roster)
        AgentInfo(
          id: id,
          name: name,
          status: active ? 'active' : 'planned',
          decisionsLastHour: active ? 12 + 37 * (++i) % 140 : 0,
          fallbackRate: active ? (i % 3) * 0.05 : 0,
          lastDecision: active ? 'demo decision by $name' : '',
          lastAt: active ? now.subtract(Duration(seconds: 7 * i)) : null,
          lastModel: !active
              ? ''
              : switch (id) {
                  'dispatch_broker' || 'ops_supervisor' => '',
                  'surge_equilibrium' => 'supply_demand_ratio',
                  _ => 'gemini/gemini-3.5-flash-lite',
                },
        ),
    ];
  }

  @override
  Future<List<AgentEvent>> recentEvents({int limit = 80, int perAgent = 8}) async => const [];

  @override
  Future<SurgeMapData> surgeMap() async {
    SurgeCell cell(String id, double dLat, double dLng, double m) {
      final lat = centerLat + dLat, lng = centerLng + dLng;
      return SurgeCell(
        cell: id,
        multiplier: m,
        boundary: hexAround(lat, lng, 0.46),
        centerLat: lat,
        centerLng: lng,
        demand: (m * 6).round(),
        supply: 2,
      );
    }

    return SurgeMapData(
      source: 'mock',
      asOf: DateTime.now().toUtc(),
      cells: [
        cell('mock-1', 0.027, -0.002, 2.4),
        cell('mock-2', 0.019, 0.004, 1.6),
        cell('mock-3', -0.012, 0.022, 1.2),
      ],
    );
  }

  @override
  Future<void> probe({bool agents = false}) async {}
}

/// A flat hexagon of circumradius [radiusKm] around (lat, lng), like an H3 cell.
List<(double, double)> hexAround(double lat, double lng, double radiusKm) {
  final kLat = radiusKm / 110.574;
  final kLng = radiusKm / (111.320 * math.cos(lat * math.pi / 180));
  return [for (var i = 0; i < 6; i++) (lat + kLat * math.sin(math.pi / 3 * i), lng + kLng * math.cos(math.pi / 3 * i))];
}
