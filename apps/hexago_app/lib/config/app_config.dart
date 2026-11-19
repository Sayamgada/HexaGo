import 'package:flutter/foundation.dart';

/// Application runtime configuration injected via --dart-define.
class AppConfig {
  /// Base HTTP URL for the dispatch-go API Gateway.
  static const String apiBaseUrl = String.fromEnvironment(
    'API_BASE_URL',
    defaultValue: 'http://localhost:8080',
  );

  /// Base WebSocket URL for real-time telemetry and dispatch events.
  static const String wsBaseUrl = String.fromEnvironment(
    'WS_BASE_URL',
    defaultValue: 'ws://localhost:8080',
  );

  /// `--dart-define=USE_MOCK=true|false` picks the Ops data source.
  /// Without the define: mock in debug/profile, live in release builds.
  static const bool useMock = bool.hasEnvironment('USE_MOCK')
      ? bool.fromEnvironment('USE_MOCK')
      : !kReleaseMode;

  static const String appName = 'HexaGo';

  /// Source repository and the latest desktop / Android release.
  static const String repoUrl = 'https://github.com/Sayamgada/HexaGo';
  static const String releasesUrl = '$repoUrl/releases/latest';
  static const String defaultCity = 'Bengaluru';
  static const double cityLat = 12.9716;
  static const double cityLng = 77.5946;
}
