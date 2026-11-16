import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/ops/data/mock_ops_source.dart';
import 'package:hexago_app/features/ops/models/ops_models.dart';
import 'package:hexago_app/features/ops/state/fleet_tracker.dart';

void main() {
  group('MockOpsSource', () {
    test('simulates 300 cars around Bengaluru with consistent metrics', () async {
      final src = MockOpsSource(seed: 42, tick: const Duration(milliseconds: 20));
      final drivers = src.drivers.first;
      final metrics = src.metrics.first;
      src.start();

      final fleet = await drivers;
      expect(fleet, hasLength(300));
      expect(fleet.map((d) => d.id).toSet(), hasLength(300));
      expect(fleet.where((d) => d.isAutonomous), isNotEmpty);
      for (final d in fleet) {
        expect(d.lat, inInclusiveRange(12.7, 13.2));
        expect(d.lng, inInclusiveRange(77.3, 77.9));
      }

      final m = await metrics;
      final active = fleet.where((d) => d.status != DriverStatus.offline).length;
      expect(m.activeDrivers, active);
      expect(m.idleDrivers, fleet.where((d) => d.status == DriverStatus.idle).length);
      expect(m.p50Ms, lessThan(m.p99Ms));
      await src.dispose();
    });

    test('safety incident flags a car and emits Safety Sentinel events', () async {
      final src = MockOpsSource(seed: 1, tick: const Duration(milliseconds: 20));
      final events = <AgentEvent>[];
      src.agentEvents.listen(events.add);
      src.start();
      await src.drivers.first;

      await src.triggerScenario(Scenario.safetyIncident);
      final next = await src.drivers.first;
      expect(next.where((d) => d.status == DriverStatus.incident), hasLength(1));
      expect(events.map((e) => e.agent), contains('Safety Sentinel'));
      await src.dispose();
    });
  });

  group('FleetTracker', () {
    test('interpolates position and heading along the shortest arc', () {
      var now = DateTime(2026);
      final tracker = FleetTracker(clock: () => now);
      tracker.apply([const DriverPosition(id: 'a', lat: 0, lng: 0, heading: 350)]);
      now = now.add(const Duration(seconds: 1));
      tracker.apply([const DriverPosition(id: 'a', lat: 1, lng: 2, heading: 10)]);

      final car = tracker.tracks.single;
      expect(tracker.progressAt(now), 0);
      final mid = now.add(const Duration(milliseconds: 500));
      final t = tracker.progressAt(mid);
      expect(t, closeTo(0.5, 1e-9));
      expect(car.latAt(t), closeTo(0.5, 1e-9));
      expect(car.lngAt(t), closeTo(1.0, 1e-9));
      expect(car.headingAt(t), closeTo(0, 1e-9));
      expect(tracker.progressAt(now.add(const Duration(seconds: 3))), 1);
    });

    test('drops cars that stop reporting', () {
      var now = DateTime(2026);
      final tracker = FleetTracker(clock: () => now, staleAfter: const Duration(seconds: 5));
      tracker.apply([
        const DriverPosition(id: 'a', lat: 0, lng: 0),
        const DriverPosition(id: 'b', lat: 0, lng: 0),
      ]);
      now = now.add(const Duration(seconds: 6));
      tracker.apply([const DriverPosition(id: 'a', lat: 0, lng: 0)]);
      expect(tracker.tracks.map((t) => t.target.id), ['a']);
    });
  });
}
