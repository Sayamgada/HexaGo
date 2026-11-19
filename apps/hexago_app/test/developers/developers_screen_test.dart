import 'package:dio/dio.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_test/flutter_test.dart';

import 'package:hexago_app/config/app_config.dart';
import 'package:hexago_app/features/developers/developer_api.dart';
import 'package:hexago_app/features/developers/developers_screen.dart';
import 'package:hexago_app/screens/home_screen.dart';
import 'package:hexago_app/util/links.dart';

class FakeDeveloperApi implements DeveloperApi {
  FakeDeveloperApi({this.fail = false});
  final bool fail;
  final calls = <({String name, List<String> scopes})>[];

  @override
  String get baseUrl => 'https://gw.example';

  @override
  Future<CreatedKey> createKey({required String name, required List<String> scopes}) async {
    calls.add((name: name, scopes: scopes));
    if (fail) throw DeveloperApiException(429, 'too many keys created from this address');
    return CreatedKey(apiKey: 'hxg_test_${'a' * 40}', id: 'key_1', prefix: 'hxg_te', scopes: scopes, rateLimitPerMin: 60);
  }
}

/// A Dio adapter answering one canned response.
class _Adapter implements HttpClientAdapter {
  _Adapter(this.status, this.body);
  final int status;
  final String body;
  RequestOptions? last;

  @override
  Future<ResponseBody> fetch(RequestOptions options, Stream<List<int>>? requestStream, Future<void>? cancelFuture) async {
    last = options;
    return ResponseBody.fromString(body, status, headers: {
      Headers.contentTypeHeader: ['application/json'],
    });
  }

  @override
  void close({bool force = false}) {}
}

void main() {
  late List<String> clipboard;
  late List<Uri> opened;

  setUp(() {
    clipboard = [];
    opened = [];
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger.setMockMethodCallHandler(SystemChannels.platform,
        (call) async {
      if (call.method == 'Clipboard.setData') clipboard.add((call.arguments as Map)['text'] as String);
      return null;
    });
  });

  Widget app(DeveloperApi api, {Widget? child}) => ProviderScope(
        overrides: [
          developerApiProvider.overrideWithValue(api),
          linkOpenerProvider.overrideWithValue((uri) async {
            opened.add(uri);
            return true;
          }),
        ],
        child: MaterialApp(home: child ?? const DevelopersScreen()),
      );

  testWidgets('creates a key with the selected scopes, shows it once and copies it', (tester) async {
    tester.view.physicalSize = const Size(1200, 2400);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.reset);
    final api = FakeDeveloperApi();
    await tester.pumpWidget(app(api));

    // curl examples use a shell variable until a key exists.
    expect(find.textContaining(r'Bearer $HEXAGO_KEY'), findsWidgets);
    await tester.enterText(find.byKey(const Key('key-name')), 'demo app');
    await tester.tap(find.byKey(const Key('scope-ledger:read')));
    await tester.pump();
    await tester.tap(find.byKey(const Key('create-key')));
    await tester.pumpAndSettle();

    expect(api.calls.single.name, 'demo app');
    expect(api.calls.single.scopes, isNot(contains('ledger:read')));
    expect(api.calls.single.scopes, hasLength(5));
    expect(find.byKey(const Key('created-key')), findsOneWidget);
    expect(find.textContaining('Shown only once'), findsOneWidget);
    expect(find.textContaining('Bearer hxg_test_'), findsWidgets);

    await tester.tap(find.byKey(const Key('copy-key')));
    await tester.pump();
    expect(clipboard.last, startsWith('hxg_test_'));
    expect(find.text('API key copied'), findsOneWidget);

    await tester.tap(find.byKey(const Key('copy-example-0')));
    await tester.pump();
    expect(clipboard.last, contains('https://gw.example/v1/rides'));
    expect(clipboard.last, contains('Bearer hxg_test_'));
  });

  testWidgets('shows the error when key creation fails, and opens /docs', (tester) async {
    tester.view.physicalSize = const Size(1200, 2400);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.reset);
    await tester.pumpWidget(app(FakeDeveloperApi(fail: true)));
    await tester.tap(find.byKey(const Key('create-key')));
    await tester.pumpAndSettle();
    expect(find.byKey(const Key('key-error')), findsOneWidget);
    expect(find.textContaining('HTTP 429'), findsOneWidget);

    await tester.tap(find.byKey(const Key('open-docs')));
    await tester.tap(find.byKey(const Key('link-docs')));
    expect(opened.map((u) => u.toString()), everyElement('https://gw.example/docs'));
    expect(opened, hasLength(2));
  });

  testWidgets('create is disabled with no scope selected', (tester) async {
    tester.view.physicalSize = const Size(1200, 2400);
    tester.view.devicePixelRatio = 1;
    addTearDown(tester.view.reset);
    await tester.pumpWidget(app(FakeDeveloperApi()));
    for (final s in apiScopes.keys) {
      await tester.tap(find.byKey(Key('scope-$s')));
      await tester.pump();
    }
    final button = tester.widget<FilledButton>(find.byKey(const Key('create-key')));
    expect(button.onPressed, isNull);
  });

  testWidgets('home screen download link opens the latest release', (tester) async {
    await tester.pumpWidget(app(FakeDeveloperApi(), child: const HomeScreen(showDownloadLink: true)));
    expect(find.byKey(const Key('role-card-Developers')), findsOneWidget);
    await tester.tap(find.byKey(const Key('download-apps')));
    expect(opened.single.toString(), AppConfig.releasesUrl);
  });

  group('DioDeveloperApi', () {
    test('posts the name and scopes and parses the key', () async {
      final adapter = _Adapter(201,
          '{"api_key":"hxg_abc","id":"key_9","prefix":"hxg_ab","scopes":["rides:read"],"rate_limit_per_min":30}');
      final dio = Dio()..httpClientAdapter = adapter;
      final api = DioDeveloperApi(apiBaseUrl: 'https://gw.example/', dio: dio);
      expect(api.baseUrl, 'https://gw.example');
      final k = await api.createKey(name: 'n', scopes: ['rides:read']);
      expect(adapter.last!.uri.toString(), 'https://gw.example/v1/developer/keys');
      expect(adapter.last!.data, {'name': 'n', 'scopes': ['rides:read']});
      expect((k.apiKey, k.id, k.prefix, k.rateLimitPerMin), ('hxg_abc', 'key_9', 'hxg_ab', 30));
      expect(k.scopes, ['rides:read']);
    });

    test('turns an error body into a DeveloperApiException', () async {
      final dio = Dio()..httpClientAdapter = _Adapter(400, '{"error":"unknown scope \\"admin\\""}');
      final api = DioDeveloperApi(apiBaseUrl: 'https://gw.example', dio: dio);
      await expectLater(
        api.createKey(name: 'n', scopes: ['admin']),
        throwsA(isA<DeveloperApiException>()
            .having((e) => e.status, 'status', 400)
            .having((e) => e.toString(), 'message', contains('unknown scope'))),
      );
    });

    test('defaults when fields are missing', () {
      final k = CreatedKey.fromJson(const {});
      expect((k.apiKey, k.rateLimitPerMin), ('', 60));
      expect(k.scopes, isEmpty);
      expect(DeveloperApiException(null, 'offline').toString(), 'offline');
    });
  });

  test('curl examples cover rides, pricing, booking, agents, rate limits and the stream', () {
    final ex = curlExamples('https://gw.example', 'hxg_k');
    expect(ex, hasLength(6));
    expect(ex.last.command, contains("wss://gw.example/v1/stream?api_key=hxg_k"));
    expect(ex.every((e) => e.command.contains('hxg_k')), isTrue);
  });
}
