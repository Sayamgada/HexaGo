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

  /// CARTO basemaps require a key on every tile request (free tier:
  /// https://carto.com/basemaps/apikey). Without it the map draws no tiles.
  static const String cartoApiKey = String.fromEnvironment('CARTO_API_KEY');

  static const String appName = 'HexaGo';
  static const String defaultCity = 'Bengaluru';
  static const double cityLat = 12.9716;
  static const double cityLng = 77.5946;
}
