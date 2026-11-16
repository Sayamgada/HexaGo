import 'dart:async';

import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../config/app_config.dart';
import '../data/live_ops_source.dart';
import '../data/mock_ops_source.dart';
import '../data/ops_source.dart';
import '../models/ops_models.dart';
import 'fleet_tracker.dart';

/// Selected by `--dart-define=USE_MOCK=true|false` (see [AppConfig.useMock]).
/// Lives only while the Ops screen is open.
final opsSourceProvider = Provider.autoDispose<OpsSource>((ref) {
  final OpsSource source = AppConfig.useMock
      ? MockOpsSource(centerLat: AppConfig.cityLat, centerLng: AppConfig.cityLng)
      : LiveOpsSource(apiBaseUrl: AppConfig.apiBaseUrl, wsBaseUrl: AppConfig.wsBaseUrl);
  source.start();
  ref.onDispose(source.dispose);
  return source;
});

/// Whether to load CARTO tiles (disabled in widget tests).
final showMapTilesProvider = Provider<bool>((ref) => true);

final fleetTrackerProvider = Provider.autoDispose<FleetTracker>((ref) {
  final source = ref.watch(opsSourceProvider);
  final tracker = FleetTracker();
  final sub = source.drivers.listen(tracker.apply);
  ref.onDispose(() {
    sub.cancel();
    tracker.dispose();
  });
  return tracker;
});

class ConnectionStatusNotifier extends Notifier<OpsConnectionStatus> {
  @override
  OpsConnectionStatus build() {
    final source = ref.watch(opsSourceProvider);
    final sub = source.status.listen((s) => state = s);
    ref.onDispose(sub.cancel);
    return source.latestStatus;
  }
}

final connectionStatusProvider =
    NotifierProvider.autoDispose<ConnectionStatusNotifier, OpsConnectionStatus>(ConnectionStatusNotifier.new);

/// Latest dispatch metrics (null until the first sample arrives).
class MetricsNotifier extends Notifier<DispatchMetrics?> {
  @override
  DispatchMetrics? build() {
    final source = ref.watch(opsSourceProvider);
    final sub = source.metrics.listen((m) => state = m);
    ref.onDispose(sub.cancel);
    return source.latestMetrics;
  }
}

final metricsProvider = NotifierProvider.autoDispose<MetricsNotifier, DispatchMetrics?>(MetricsNotifier.new);

class FeedState {
  const FeedState({this.events = const [], this.totalSeen = 0});

  /// Newest first, capped at [AgentFeedNotifier.maxEvents].
  final List<AgentEvent> events;

  /// All events received this session (not capped).
  final int totalSeen;
}

/// Buffers incoming agent events and flushes them at most every 250 ms so
/// bursts cause one rebuild instead of many.
class AgentFeedNotifier extends Notifier<FeedState> {
  static const maxEvents = 150;
  static const flushInterval = Duration(milliseconds: 250);

  final List<AgentEvent> _pending = [];
  Timer? _flush;

  @override
  FeedState build() {
    final source = ref.watch(opsSourceProvider);
    final sub = source.agentEvents.listen(_onEvent);
    ref.onDispose(() {
      sub.cancel();
      _flush?.cancel();
      _flush = null;
      _pending.clear();
    });
    return const FeedState();
  }

  void _onEvent(AgentEvent e) {
    _pending.add(e);
    _flush ??= Timer(flushInterval, _drain);
  }

  void _drain() {
    _flush = null;
    if (_pending.isEmpty) return;
    final incoming = _pending.reversed.toList();
    _pending.clear();
    final merged = [...incoming, ...state.events];
    state = FeedState(
      events: merged.length > maxEvents ? merged.sublist(0, maxEvents) : merged,
      totalSeen: state.totalSeen + incoming.length,
    );
  }
}

final agentFeedProvider = NotifierProvider.autoDispose<AgentFeedNotifier, FeedState>(AgentFeedNotifier.new);

/// Autonomous decisions today = rider/driver matches (from the dispatch
/// counter) + agent decisions streamed during this session.
final decisionsTodayProvider = Provider.autoDispose<int>((ref) {
  final matches = ref.watch(metricsProvider.select((m) => m?.matchesTotal ?? 0));
  final events = ref.watch(agentFeedProvider.select((f) => f.totalSeen));
  return matches + events;
});
