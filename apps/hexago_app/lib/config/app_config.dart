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

  static const String appName = 'HexaGo';
  static const String defaultCity = 'Bengaluru';
  static const double cityLat = 12.9716;
  static const double cityLng = 77.5946;
}

