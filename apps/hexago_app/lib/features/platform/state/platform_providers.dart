import 'dart:async';

import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../config/app_config.dart';
import '../data/platform_api.dart';
import '../models/platform_models.dart';

final platformApiProvider = Provider<PlatformApi>(
  (ref) => AppConfig.useMock
      ? MockPlatformApi(centerLat: AppConfig.cityLat, centerLng: AppConfig.cityLng)
      : DioPlatformApi(apiBaseUrl: AppConfig.apiBaseUrl),
);

/// How often the Agents tab and the surge map refresh (overridable in tests).
final agentsRefreshProvider = Provider<Duration>((ref) => const Duration(seconds: 10));
final surgeRefreshProvider = Provider<Duration>((ref) => const Duration(seconds: 15));

/// `GET /v1/agents`, every 10 s while the Agents tab is open. Keeps the last
/// good roster when a refresh fails.
class AgentRosterNotifier extends AsyncNotifier<List<AgentInfo>> {
  Timer? _timer;

  @override
  Future<List<AgentInfo>> build() {
    _timer?.cancel();
    _timer = Timer.periodic(ref.read(agentsRefreshProvider), (_) => unawaited(refreshNow()));
    ref.onDispose(() => _timer?.cancel());
    return ref.read(platformApiProvider).agents();
  }

  Future<void> refreshNow() async {
    try {
      final v = await ref.read(platformApiProvider).agents();
      state = AsyncData(v);
    } catch (e, st) {
      if (!state.hasValue) state = AsyncError(e, st);
    }
  }
}

/// No automatic retry: the notifier polls on its own schedule.
Duration? _noRetry(int count, Object error) => null;

final agentRosterProvider = AsyncNotifierProvider.autoDispose<AgentRosterNotifier, List<AgentInfo>>(
  AgentRosterNotifier.new,
  retry: _noRetry,
);

/// `GET /v1/pricing/surge-map`, every 15 s while a map shows it (the agent
/// recomputes every 30 s). Keeps the last good map when a refresh fails.
class SurgeMapNotifier extends AsyncNotifier<SurgeMapData> {
  Timer? _timer;

  @override
  Future<SurgeMapData> build() {
    _timer?.cancel();
    _timer = Timer.periodic(ref.read(surgeRefreshProvider), (_) => unawaited(refreshNow()));
    ref.onDispose(() => _timer?.cancel());
    return ref.read(platformApiProvider).surgeMap();
  }

  Future<void> refreshNow() async {
    try {
      final v = await ref.read(platformApiProvider).surgeMap();
      state = AsyncData(v);
    } catch (e, st) {
      if (!state.hasValue) state = AsyncError(e, st);
    }
  }
}

final surgeMapProvider = AsyncNotifierProvider.autoDispose<SurgeMapNotifier, SurgeMapData>(
  SurgeMapNotifier.new,
  retry: _noRetry,
);

/// The map legend's "Surge zones" toggle (shared by the Ops and Rider maps).
class ShowSurgeNotifier extends Notifier<bool> {
  @override
  bool build() => true;
  void toggle() => state = !state;
}

final showSurgeProvider = NotifierProvider<ShowSurgeNotifier, bool>(ShowSurgeNotifier.new);
