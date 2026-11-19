import 'dart:async';
import 'dart:typed_data';

import 'package:dio/dio.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../config/app_config.dart';
import '../../platform/state/platform_providers.dart';
import '../data/basemap.dart';
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

/// Whether to load basemap tiles (disabled in widget tests).
final showMapTilesProvider = Provider<bool>((ref) => true);

/// The basemap for this session: keyless CARTO dark tiles if they serve real
/// imagery, otherwise OSM (see [probeBasemap]). Probed once.
final basemapProvider = FutureProvider<Basemap>((ref) {
  final dio = Dio(BaseOptions(
    responseType: ResponseType.bytes,
    connectTimeout: const Duration(seconds: 5),
    receiveTimeout: const Duration(seconds: 5),
  ));
  ref.onDispose(dio.close);
  return probeBasemap((url) async {
    final r = await dio.get<List<int>>(url);
    return Uint8List.fromList(r.data ?? const []);
  });
});

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

  bool _disposed = false;

  @override
  FeedState build() {
    final source = ref.watch(opsSourceProvider);
    final sub = source.agentEvents.listen(_onEvent);
    _disposed = false;
    ref.onDispose(() {
      _disposed = true;
      sub.cancel();
      _flush?.cancel();
      _flush = null;
      _pending.clear();
    });
    unawaited(_seed());
    return const FeedState();
  }

  /// Fills the feed with recent decisions of every agent (at most a few per
  /// agent, so the busy Dispatch Broker cannot crowd out the others) below
  /// the live events.
  Future<void> _seed() async {
    final List<AgentEvent> history;
    try {
      history = await ref.read(platformApiProvider).recentEvents(limit: 80, perAgent: 8);
    } catch (_) {
      return; // agents-py asleep: the live stream fills the feed
    }
    if (_disposed || history.isEmpty) return;
    final have = {for (final e in state.events) e.id};
    final older = [for (final e in history) if (!have.contains(e.id)) e]..sort((a, b) => b.ts.compareTo(a.ts));
    final merged = [...state.events, ...older];
    state = FeedState(
      events: merged.length > maxEvents ? merged.sublist(0, maxEvents) : merged,
      totalSeen: state.totalSeen + older.length,
    );
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

/// Autonomous decisions today: the server's persistent count (every row in
/// agent_events since midnight IST, so it survives restarts). Servers without
/// it (and the offline mock) fall back to matches + events seen this session.
final decisionsTodayProvider = Provider.autoDispose<int>((ref) {
  final persisted = ref.watch(metricsProvider.select((m) => m?.decisionsToday));
  if (persisted != null) return persisted;
  final matches = ref.watch(metricsProvider.select((m) => m?.matchesTotal ?? 0));
  final events = ref.watch(agentFeedProvider.select((f) => f.totalSeen));
  return matches + events;
});
