import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/ops/models/ops_models.dart';

void main() {
  group('REST GET /v1/metrics/dispatch', () {
    test('parses every field', () {
      final m = DispatchMetrics.fromJson(jsonDecode('''
        {"p50_ms": 12.5, "p95_ms": 38, "p99_ms": 61.25, "matches_total": 98765,
         "active_drivers": 300, "idle_drivers": 110, "open_requests": 42, "rides_in_progress": 150}
      ''') as Map<String, dynamic>);
      expect(m.p50Ms, 12.5);
      expect(m.p95Ms, 38.0);
      expect(m.p99Ms, 61.25);
      expect(m.matchesTotal, 98765);
      expect(m.activeDrivers, 300);
      expect(m.idleDrivers, 110);
      expect(m.openRequests, 42);
      expect(m.ridesInProgress, 150);
    });

    test('coerces numeric strings and floats, defaults missing to 0', () {
      final m = DispatchMetrics.fromJson({'p50_ms': '7.5', 'matches_total': 10.0, 'active_drivers': '12'});
      expect(m.p50Ms, 7.5);
      expect(m.matchesTotal, 10);
      expect(m.activeDrivers, 12);
      expect(m.p99Ms, 0);
      expect(m.openRequests, 0);
    });

    test('round-trips through toJson', () {
      const m = DispatchMetrics(
        p50Ms: 1, p95Ms: 2, p99Ms: 3, matchesTotal: 4,
        activeDrivers: 5, idleDrivers: 6, openRequests: 7, ridesInProgress: 8,
      );
      final back = DispatchMetrics.fromJson(m.toJson());
      expect(back.toJson(), m.toJson());
    });
  });

  group('WebSocket topic "drivers"', () {
    test('parses a driver list with every field', () {
      final msg = parseOpsMessage(jsonEncode({
        'topic': 'drivers',
        'data': [
          {
            'id': 'drv_001', 'lat': 12.97, 'lng': 77.59, 'heading': 270, 'speed_kmh': 31.5,
            'status': 'on_trip', 'tier': 'av', 'battery': 82,
          },
          {
            'id': 7, 'lat': '12.9', 'lng': '77.6', 'heading': 0, 'speed_kmh': 0,
            'status': 'idle', 'tier': 'economy', 'battery': null,
          },
        ],
      }));
      expect(msg, isA<DriversMessage>());
      final drivers = (msg as DriversMessage).drivers;
      expect(drivers, hasLength(2));
      final a = drivers[0];
      expect(a.id, 'drv_001');
      expect(a.lat, 12.97);
      expect(a.lng, 77.59);
      expect(a.heading, 270);
      expect(a.speedKmh, 31.5);
      expect(a.status, DriverStatus.onTrip);
      expect(a.tier, 'av');
      expect(a.isAutonomous, isTrue);
      expect(a.battery, 82);
      final b = drivers[1];
      expect(b.id, '7');
      expect(b.lat, 12.9);
      expect(b.status, DriverStatus.idle);
      expect(b.isAutonomous, isFalse);
      expect(b.battery, isNull);
    });

    test('maps status synonyms and unknown values', () {
      expect(DriverStatus.fromWire('en_route'), DriverStatus.toPickup);
      expect(DriverStatus.fromWire('busy'), DriverStatus.onTrip);
      expect(DriverStatus.fromWire('OFFLINE'), DriverStatus.offline);
      expect(DriverStatus.fromWire('sos'), DriverStatus.incident);
      expect(DriverStatus.fromWire('teleporting'), DriverStatus.unknown);
      expect(DriverStatus.fromWire(null), DriverStatus.unknown);
    });

    test('skips entries without an id', () {
      final msg = parseOpsMessage({
        'topic': 'drivers',
        'data': [
          {'lat': 1, 'lng': 2},
          {'id': 'x', 'lat': 1, 'lng': 2},
        ],
      }) as DriversMessage;
      expect(msg.drivers.single.id, 'x');
    });

    test('returns null when data is not a list', () {
      expect(parseOpsMessage({'topic': 'drivers', 'data': {}}), isNull);
    });
  });

  group('WebSocket topic "ops" type "agent_event"', () {
    test('parses every field', () {
      final msg = parseOpsMessage(jsonEncode({
        'topic': 'ops',
        'data': {
          'type': 'agent_event',
          'agent': 'surge_equilibrium',
          'decision': 'Set Koramangala multiplier to 1.4x',
          'reasoning': 'Demand/supply ratio rose to 1.8',
          'confidence': 0.87,
          'latency_ms': 412.5,
          'used_fallback': true,
          'ts': '2026-11-16T09:30:00Z',
        },
      }));
      expect(msg, isA<AgentEventMessage>());
      final e = (msg as AgentEventMessage).event;
      expect(e.agent, 'surge_equilibrium');
      expect(e.decision, 'Set Koramangala multiplier to 1.4x');
      expect(e.reasoning, 'Demand/supply ratio rose to 1.8');
      expect(e.confidence, 0.87);
      expect(e.latencyMs, 412.5);
      expect(e.usedFallback, isTrue);
      expect(e.ts, DateTime.utc(2026, 11, 16, 9, 30));
    });

    test('accepts epoch seconds / millis and percentage confidence', () {
      final secs = AgentEvent.fromJson({'agent': 'a', 'ts': 1700000000, 'confidence': 87});
      expect(secs.ts, DateTime.fromMillisecondsSinceEpoch(1700000000000, isUtc: true));
      expect(secs.confidence, closeTo(0.87, 1e-9));
      final millis = AgentEvent.fromJson({'agent': 'a', 'ts': 1700000000123});
      expect(millis.ts.millisecondsSinceEpoch, 1700000000123);
      expect(millis.usedFallback, isFalse);
    });

    test('round-trips through toJson', () {
      final e = AgentEvent(
        agent: 'Safety Sentinel', decision: 'd', reasoning: 'r', confidence: 0.5,
        latencyMs: 9, usedFallback: false, ts: DateTime.utc(2026, 1, 2, 3, 4, 5),
      );
      final back = (parseOpsMessage({'topic': 'ops', 'data': e.toJson()}) as AgentEventMessage).event;
      expect(back.toJson(), e.toJson());
    });
  });

  group('WebSocket topic "ops" type "metrics"', () {
    test('parses the same fields as the REST endpoint', () {
      final msg = parseOpsMessage(
        '{"topic":"ops","data":{"type":"metrics","p50_ms":18,"p95_ms":33,"p99_ms":49.9,'
        '"matches_total":5,"active_drivers":4,"idle_drivers":3,"open_requests":2,"rides_in_progress":1}}',
      );
      expect(msg, isA<MetricsMessage>());
      final m = (msg as MetricsMessage).metrics;
      expect(m.p50Ms, 18);
      expect(m.p95Ms, 33);
      expect(m.p99Ms, 49.9);
      expect(m.matchesTotal, 5);
      expect(m.activeDrivers, 4);
      expect(m.idleDrivers, 3);
      expect(m.openRequests, 2);
      expect(m.ridesInProgress, 1);
    });
  });

  group('malformed and unknown frames', () {
    test('return null instead of throwing', () {
      expect(parseOpsMessage('not json'), isNull);
      expect(parseOpsMessage('[1,2,3]'), isNull);
      expect(parseOpsMessage(null), isNull);
      expect(parseOpsMessage({'topic': 'weather', 'data': {}}), isNull);
      expect(parseOpsMessage({'topic': 'ops', 'data': {'type': 'heartbeat'}}), isNull);
      expect(parseOpsMessage({'topic': 'ops', 'data': 'oops'}), isNull);
    });
  });
}
