import 'dart:async';
import 'dart:convert';
import 'dart:math' as math;

import 'package:dio/dio.dart';
import 'package:web_socket_channel/web_socket_channel.dart';

import '../models/ops_models.dart';
import 'ops_source.dart';

/// Minimal socket abstraction so reconnect logic is testable without a server.
abstract class WsConnection {
  /// Completes when the handshake succeeds; errors if it fails.
  Future<void> get ready;
  Stream<dynamic> get stream;
  void send(String message);
  Future<void> close();
}

typedef WsConnector = WsConnection Function(Uri uri);

class _ChannelConnection implements WsConnection {
  _ChannelConnection(Uri uri) : _channel = WebSocketChannel.connect(uri);
  final WebSocketChannel _channel;

  @override
  Future<void> get ready => _channel.ready;
  @override
  Stream<dynamic> get stream => _channel.stream;
  @override
  void send(String message) => _channel.sink.add(message);
  @override
  Future<void> close() => _channel.sink.close();
}

WsConnection defaultWsConnector(Uri uri) => _ChannelConnection(uri);

/// Exponential backoff: 1 s, 2 s, 4 s ... capped at 30 s.
Duration defaultBackoff(int attempt) =>
    Duration(milliseconds: math.min(30000, 1000 * math.pow(2, attempt).toInt()));

/// Real backend over REST (dio) and WebSocket (web_socket_channel).
class LiveOpsSource implements OpsSource {
  LiveOpsSource({
    required String apiBaseUrl,
    required String wsBaseUrl,
    Dio? dio,
    WsConnector? connector,
    Duration Function(int attempt)? backoff,
    this.handshakeTimeout = const Duration(seconds: 75),
  })  : _api = _trimSlash(apiBaseUrl),
        _ws = _trimSlash(wsBaseUrl),
        _dio = dio ??
            Dio(BaseOptions(
              // Render free tier can take ~1 min to cold-start.
              connectTimeout: const Duration(seconds: 90),
              receiveTimeout: const Duration(seconds: 90),
            )),
        _connector = connector ?? defaultWsConnector,
        _backoff = backoff ?? defaultBackoff;

  static String _trimSlash(String s) => s.endsWith('/') ? s.substring(0, s.length - 1) : s;

  final String _api;
  final String _ws;
  final Dio _dio;
  final WsConnector _connector;
  final Duration Function(int attempt) _backoff;
  final Duration handshakeTimeout;

  final _drivers = StreamController<List<DriverPosition>>.broadcast();
  final _events = StreamController<AgentEvent>.broadcast();
  final _metrics = StreamController<DispatchMetrics>.broadcast();
  final _status = StreamController<OpsConnectionStatus>.broadcast();

  OpsConnectionStatus _latestStatus = OpsConnectionStatus.waking;
  DispatchMetrics? _latestMetrics;
  WsConnection? _conn;
  StreamSubscription<dynamic>? _sub;
  Timer? _retryTimer;
  int _attempt = 0;
  int _generation = 0;
  bool _everLive = false;
  bool _started = false;
  bool _disposed = false;

  /// Number of reconnect attempts scheduled since the last good connection.
  int get attempt => _attempt;

  Uri get streamUri => Uri.parse('$_ws/v1/stream');

  @override
  String get modeLabel => 'LIVE';
  @override
  Stream<List<DriverPosition>> get drivers => _drivers.stream;
  @override
  Stream<AgentEvent> get agentEvents => _events.stream;
  @override
  Stream<DispatchMetrics> get metrics => _metrics.stream;
  @override
  Stream<OpsConnectionStatus> get status => _status.stream;
  @override
  OpsConnectionStatus get latestStatus => _latestStatus;
  @override
  DispatchMetrics? get latestMetrics => _latestMetrics;

  @override
  void start() {
    if (_started || _disposed) return;
    _started = true;
    unawaited(_seedMetrics());
    _connect();
  }

  Future<void> _seedMetrics() async {
    try {
      final metrics = await fetchMetrics();
      if (!_disposed && _latestMetrics == null) _emitMetrics(metrics);
    } catch (_) {
      // The stream will deliver metrics once the backend is awake.
    }
  }

  /// `GET /v1/metrics/dispatch`.
  Future<DispatchMetrics> fetchMetrics() async {
    final res = await _dio.get<dynamic>('$_api/v1/metrics/dispatch');
    final data = res.data is String ? jsonDecode(res.data as String) : res.data;
    return DispatchMetrics.fromJson(Map<String, dynamic>.from(data as Map));
  }

  /// `POST /v1/sim/scenario/{name}`.
  @override
  Future<void> triggerScenario(Scenario scenario) async {
    await _dio.post<dynamic>('$_api/v1/sim/scenario/${scenario.wire}');
  }

  void _setStatus(OpsConnectionStatus s) {
    if (_latestStatus == s || _disposed) return;
    _latestStatus = s;
    _status.add(s);
  }

  void _emitMetrics(DispatchMetrics m) {
    _latestMetrics = m;
    _metrics.add(m);
  }

  void _connect() {
    if (_disposed) return;
    final gen = ++_generation;
    _setStatus(_everLive ? OpsConnectionStatus.reconnecting : OpsConnectionStatus.waking);

    final WsConnection conn;
    try {
      conn = _connector(streamUri);
    } catch (_) {
      _scheduleReconnect(gen);
      return;
    }
    _conn = conn;

    _sub = conn.stream.listen(
      (frame) {
        if (gen != _generation) return;
        _markLive();
        _handleFrame(frame);
      },
      onError: (Object _) => _scheduleReconnect(gen),
      onDone: () => _scheduleReconnect(gen),
      cancelOnError: true,
    );

    conn.ready.timeout(handshakeTimeout).then((_) {
      if (gen != _generation || _disposed) return;
      conn.send(jsonEncode({
        'action': 'subscribe',
        'topics': ['drivers', 'ops'],
      }));
      _markLive();
    }, onError: (Object _) => _scheduleReconnect(gen));
  }

  void _markLive() {
    _everLive = true;
    _attempt = 0;
    _setStatus(OpsConnectionStatus.live);
  }

  void _handleFrame(dynamic frame) {
    final msg = parseOpsMessage(frame);
    switch (msg) {
      case DriversMessage(:final drivers):
        _drivers.add(drivers);
      case AgentEventMessage(:final event):
        _events.add(event);
      case MetricsMessage(:final metrics):
        _emitMetrics(metrics);
      case null:
        break;
    }
  }

  void _scheduleReconnect(int gen) {
    if (gen != _generation || _disposed) return;
    // Invalidate this connection so late callbacks are ignored.
    _generation++;
    unawaited(_sub?.cancel());
    _sub = null;
    final old = _conn;
    _conn = null;
    unawaited(old?.close().catchError((_) {}));
    _setStatus(_everLive ? OpsConnectionStatus.reconnecting : OpsConnectionStatus.waking);
    final delay = _backoff(_attempt);
    _attempt++;
    _retryTimer?.cancel();
    _retryTimer = Timer(delay, _connect);
  }

  @override
  Future<void> dispose() async {
    _disposed = true;
    _retryTimer?.cancel();
    await _sub?.cancel();
    await _conn?.close().catchError((_) {});
    _dio.close(force: true);
    await Future.wait([
      _drivers.close(),
      _events.close(),
      _metrics.close(),
      _status.close(),
    ]);
  }
}
