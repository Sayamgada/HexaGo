import 'dart:async';
import 'dart:convert';
import 'dart:typed_data';

import 'package:dio/dio.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:hexago_app/features/ops/data/live_ops_source.dart';
import 'package:hexago_app/features/ops/models/ops_models.dart';

class FakeConnection implements WsConnection {
  FakeConnection(this.uri, {bool failHandshake = false}) {
    failHandshake ? _ready.completeError(Exception('503 waking')) : _ready.complete();
  }

  final Uri uri;
  final _ready = Completer<void>();
  final ctrl = StreamController<dynamic>();
  final sent = <String>[];
  bool closed = false;

  @override
  Future<void> get ready => _ready.future;
  @override
  Stream<dynamic> get stream => ctrl.stream;
  @override
  void send(String message) => sent.add(message);
  @override
  Future<void> close() async {
    closed = true;
  }
}

/// Records requests and answers from a route table.
class FakeAdapter implements HttpClientAdapter {
  FakeAdapter(this.routes);
  final Map<String, (int, Object?)> routes;
  final requests = <String>[];

  @override
  Future<ResponseBody> fetch(RequestOptions options, Stream<Uint8List>? requestStream, Future<void>? cancelFuture) async {
    final key = '${options.method} ${options.uri}';
    requests.add(key);
    final (status, body) = routes[key] ?? (404, {'error': 'not found'});
    return ResponseBody.fromString(jsonEncode(body), status, headers: {
      Headers.contentTypeHeader: [Headers.jsonContentType],
    });
  }

  @override
  void close({bool force = false}) {}
}

void main() {
  const metricsJson = {
    'p50_ms': 21, 'p95_ms': 40, 'p99_ms': 48, 'matches_total': 777,
    'active_drivers': 300, 'idle_drivers': 100, 'open_requests': 12, 'rides_in_progress': 180,
  };

  late List<FakeConnection> conns;
  late FakeAdapter adapter;
  late LiveOpsSource source;
  var failNext = 0;

  LiveOpsSource build() {
    final dio = Dio()..httpClientAdapter = adapter;
    return LiveOpsSource(
      apiBaseUrl: 'https://dispatch.example/',
      wsBaseUrl: 'wss://dispatch.example',
      dio: dio,
      connector: (uri) {
        final c = FakeConnection(uri, failHandshake: failNext > 0);
        if (failNext > 0) failNext--;
        conns.add(c);
        return c;
      },
      backoff: (attempt) => Duration(milliseconds: 10 * (1 << attempt)),
    );
  }

  setUp(() {
    conns = [];
    failNext = 0;
    adapter = FakeAdapter({
      'GET https://dispatch.example/v1/metrics/dispatch': (200, metricsJson),
      'POST https://dispatch.example/v1/sim/scenario/rush_hour': (202, {'ok': true}),
      'POST https://dispatch.example/v1/sim/scenario/rain_storm': (202, {'ok': true}),
      'POST https://dispatch.example/v1/sim/scenario/concert_exit': (202, {'ok': true}),
      'POST https://dispatch.example/v1/sim/scenario/safety_incident': (202, {'ok': true}),
    });
    source = build();
  });

  tearDown(() => source.dispose());

  test('connects to /v1/stream and subscribes to drivers + ops', () async {
    expect(source.latestStatus, OpsConnectionStatus.waking);
    source.start();
    await pumpEventQueue();
    expect(conns.single.uri.toString(), 'wss://dispatch.example/v1/stream');
    expect(jsonDecode(conns.single.sent.single), {
      'action': 'subscribe',
      'topics': ['drivers', 'ops'],
    });
    expect(source.latestStatus, OpsConnectionStatus.live);
  });

  test('seeds metrics from REST GET /v1/metrics/dispatch', () async {
    final first = source.metrics.first;
    source.start();
    final m = await first;
    expect(m.matchesTotal, 777);
    expect(source.latestMetrics!.p99Ms, 48);
    expect(adapter.requests, contains('GET https://dispatch.example/v1/metrics/dispatch'));
  });

  test('routes each frame type to its stream', () async {
    source.start();
    await pumpEventQueue();
    final drivers = source.drivers.first;
    final event = source.agentEvents.first;
    final metrics = source.metrics.firstWhere((m) => m.matchesTotal == 1);

    final c = conns.single.ctrl;
    c.add(jsonEncode({
      'topic': 'drivers',
      'data': [
        {'id': 'd1', 'lat': 12.9, 'lng': 77.6, 'heading': 90, 'speed_kmh': 20, 'status': 'idle', 'tier': 'av', 'battery': 70},
      ],
    }));
    c.add('garbage frame is ignored');
    c.add(jsonEncode({
      'topic': 'ops',
      'data': {
        'type': 'agent_event', 'agent': 'pool_batching', 'decision': 'pooled', 'reasoning': 'overlap',
        'confidence': 0.8, 'latency_ms': 100, 'used_fallback': false, 'ts': '2026-11-16T00:00:00Z',
      },
    }));
    c.add(jsonEncode({'topic': 'ops', 'data': {...metricsJson, 'type': 'metrics', 'matches_total': 1}}));

    expect((await drivers).single.isAutonomous, isTrue);
    expect((await event).agent, 'pool_batching');
    expect((await metrics).activeDrivers, 300);
  });

  test('reconnects with exponential backoff after the socket drops', () async {
    final statuses = <OpsConnectionStatus>[];
    source.status.listen(statuses.add);
    source.start();
    await pumpEventQueue();
    expect(source.latestStatus, OpsConnectionStatus.live);

    await conns.first.ctrl.close();
    await pumpEventQueue();
    expect(source.latestStatus, OpsConnectionStatus.reconnecting);
    expect(conns.first.closed, isTrue);

    await Future<void>.delayed(const Duration(milliseconds: 30));
    expect(conns, hasLength(2));
    expect(source.latestStatus, OpsConnectionStatus.live);
    expect(conns[1].sent, hasLength(1), reason: 're-subscribes on the new socket');
    expect(statuses, [OpsConnectionStatus.live, OpsConnectionStatus.reconnecting, OpsConnectionStatus.live]);
    expect(source.attempt, 0, reason: 'backoff resets after a good connection');
  });

  test('stays "waking" while the cold-starting backend refuses handshakes', () async {
    failNext = 2;
    source.start();
    await pumpEventQueue();
    expect(source.latestStatus, OpsConnectionStatus.waking);
    expect(source.attempt, 1);

    await Future<void>.delayed(const Duration(milliseconds: 15));
    expect(conns, hasLength(2));
    expect(source.latestStatus, OpsConnectionStatus.waking);
    expect(source.attempt, 2);

    // Second retry waits 20 ms, then succeeds.
    await Future<void>.delayed(const Duration(milliseconds: 40));
    expect(conns, hasLength(3));
    expect(source.latestStatus, OpsConnectionStatus.live);
    expect(source.attempt, 0);
  });

  test('scenario buttons POST /v1/sim/scenario/{name}', () async {
    for (final s in Scenario.values) {
      await source.triggerScenario(s);
    }
    expect(adapter.requests, [
      'POST https://dispatch.example/v1/sim/scenario/rush_hour',
      'POST https://dispatch.example/v1/sim/scenario/rain_storm',
      'POST https://dispatch.example/v1/sim/scenario/concert_exit',
      'POST https://dispatch.example/v1/sim/scenario/safety_incident',
    ]);
  });

  test('stops reconnecting after dispose', () async {
    source.start();
    await pumpEventQueue();
    await source.dispose();
    await Future<void>.delayed(const Duration(milliseconds: 50));
    expect(conns, hasLength(1));
  });

  test('default backoff doubles from 1 s and caps at 30 s', () {
    expect(defaultBackoff(0), const Duration(seconds: 1));
    expect(defaultBackoff(1), const Duration(seconds: 2));
    expect(defaultBackoff(4), const Duration(seconds: 16));
    expect(defaultBackoff(5), const Duration(seconds: 30));
    expect(defaultBackoff(12), const Duration(seconds: 30));
  });
}
