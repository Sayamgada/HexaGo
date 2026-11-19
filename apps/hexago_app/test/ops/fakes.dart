import 'dart:async';

import 'package:hexago_app/features/ops/data/ops_source.dart';
import 'package:hexago_app/features/ops/models/ops_models.dart';

/// Timer-free OpsSource for widget tests; push data through the controllers.
class FakeOpsSource implements OpsSource {
  FakeOpsSource({this.initialStatus = OpsConnectionStatus.live, this.initialMetrics});

  final OpsConnectionStatus initialStatus;
  final DispatchMetrics? initialMetrics;
  final driversCtrl = StreamController<List<DriverPosition>>.broadcast();
  final eventsCtrl = StreamController<AgentEvent>.broadcast();
  final metricsCtrl = StreamController<DispatchMetrics>.broadcast();
  final statusCtrl = StreamController<OpsConnectionStatus>.broadcast();
  final triggered = <Scenario>[];
  bool started = false;
  bool disposed = false;

  @override
  String get modeLabel => 'FAKE';
  @override
  Stream<List<DriverPosition>> get drivers => driversCtrl.stream;
  @override
  Stream<AgentEvent> get agentEvents => eventsCtrl.stream;
  @override
  Stream<DispatchMetrics> get metrics => metricsCtrl.stream;
  @override
  Stream<OpsConnectionStatus> get status => statusCtrl.stream;
  @override
  OpsConnectionStatus get latestStatus => initialStatus;
  @override
  DispatchMetrics? get latestMetrics => initialMetrics;

  @override
  void start() => started = true;

  @override
  Future<void> triggerScenario(Scenario scenario) async => triggered.add(scenario);

  @override
  Future<void> dispose() async {
    disposed = true;
  }
}

const sampleMetrics = DispatchMetrics(
  p50Ms: 23.4,
  p95Ms: 41,
  p99Ms: 87.2,
  matchesTotal: 12345,
  activeDrivers: 287,
  idleDrivers: 96,
  openRequests: 41,
  ridesInProgress: 152,
);

AgentEvent sampleEvent({
  String agent = 'dispatch_broker',
  String decision = 'Matched rider in Koramangala to drv_042 (ETA 3 min)',
  String reasoning = 'Searched H3 k-ring 2; ranked 7 idle candidates by ETA.',
  double confidence = 0.91,
  double latencyMs = 142,
  bool usedFallback = false,
  String? id,
  String kind = '',
  String rideId = '',
  String driverId = '',
  String tier = '',
  String requestedTier = '',
  bool upgraded = false,
  DateTime? ts,
}) =>
    AgentEvent(
      id: id,
      kind: kind,
      rideId: rideId,
      driverId: driverId,
      tier: tier,
      requestedTier: requestedTier,
      upgraded: upgraded,
      agent: agent,
      decision: decision,
      reasoning: reasoning,
      confidence: confidence,
      latencyMs: latencyMs,
      usedFallback: usedFallback,
      ts: ts ?? DateTime.utc(2026, 11, 16, 9, 30, 0),
    );
