import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../config/app_config.dart';
import '../../ops/models/ops_models.dart';
import '../../ops/state/ops_providers.dart';
import '../data/driver_api.dart';
import '../data/takeover_api.dart';

/// Mock (offline demo) or the live gateway, like the other features.
final driverApiProvider = Provider<DriverApi>((ref) => AppConfig.useMock
    ? MockDriverApi(centerLat: AppConfig.cityLat, centerLng: AppConfig.cityLng)
    : DioDriverApi(apiBaseUrl: AppConfig.apiBaseUrl));

/// How often the driver home and earnings refresh (overridable in tests).
final driverPollProvider = Provider<Duration>((ref) => const Duration(seconds: 2));
final earningsPollProvider = Provider<Duration>((ref) => const Duration(seconds: 6));

/// The shared `drivers` stream (the Ops fleet feed). The driver home reads its
/// car's position from it between detail polls. Overridden with an empty
/// stream in tests.
final driverPositionsProvider = Provider.autoDispose<Stream<List<DriverPosition>>>(
  (ref) => ref.watch(opsSourceProvider).drivers,
);

/// Take-over mode, Demand Forecast heatmap and routes (mock or the live gateway).
final takeoverApiProvider = Provider<TakeoverApi>((ref) => AppConfig.useMock
    ? MockTakeoverApi(centerLat: AppConfig.cityLat, centerLng: AppConfig.cityLng)
    : DioTakeoverApi(apiBaseUrl: AppConfig.apiBaseUrl, wsBaseUrl: AppConfig.wsBaseUrl));

/// How often the forecast heatmap refreshes and the take-over is renewed (overridable in tests).
final forecastPollProvider = Provider<Duration>((ref) => const Duration(seconds: 30));
final takeoverRenewProvider = Provider<Duration>((ref) => const Duration(minutes: 1));
