import '../models/ops_models.dart';

/// Data source for the Ops Command Center.
///
/// Two implementations:
/// * [MockOpsSource] simulates a Bengaluru fleet locally.
/// * [LiveOpsSource] talks to the dispatch-go gateway.
///
/// Streams are broadcast; `latest*` getters let late subscribers seed state.
abstract class OpsSource {
  /// Short label shown in the UI (e.g. `LIVE`, `MOCK`).
  String get modeLabel;

  Stream<List<DriverPosition>> get drivers;
  Stream<AgentEvent> get agentEvents;
  Stream<DispatchMetrics> get metrics;
  Stream<OpsConnectionStatus> get status;

  OpsConnectionStatus get latestStatus;
  DispatchMetrics? get latestMetrics;

  /// Starts streaming. Safe to call once.
  void start();

  /// Injects a what-if scenario. Throws on failure.
  Future<void> triggerScenario(Scenario scenario);

  Future<void> dispose();
}
