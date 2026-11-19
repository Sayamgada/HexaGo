import 'dart:async';
import 'dart:convert';
import 'dart:math' as math;

import 'package:dio/dio.dart';
import 'package:web_socket_channel/web_socket_channel.dart';

import '../models/takeover_models.dart';

/// Take-over mode, the Demand Forecast heatmap and road routes, via the gateway.
abstract class TakeoverApi {
  Future<ForecastHeatmap> heatmap(int horizon);
  Future<TakeoverState> takeOver(String driverId);
  Future<void> release(String driverId);

  /// DEMO_MODE: a ride request near the driver, assigned to it (offered on the next tick).
  Future<void> sendRide(String driverId);
  Future<OfferResult> respond(String rideId, {required bool accept});
  Future<RoutePath> route(double fromLat, double fromLng, double toLat, double toLng);

  /// `ride_offer` / `offer_closed` messages on the `driver:<id>` topic.
  Stream<RideOffer> offers(String driverId);
}

Map<String, dynamic> _json(Object? data) {
  final d = data is String ? jsonDecode(data) : data;
  return d is Map ? Map<String, dynamic>.from(d) : <String, dynamic>{};
}

String _trim(String u) => u.endsWith('/') ? u.substring(0, u.length - 1) : u;

class DioTakeoverApi implements TakeoverApi {
  DioTakeoverApi({required String apiBaseUrl, required String wsBaseUrl, Dio? dio})
      : _api = _trim(apiBaseUrl),
        _ws = _trim(wsBaseUrl),
        _dio = dio ?? Dio(BaseOptions(connectTimeout: const Duration(seconds: 60), receiveTimeout: const Duration(seconds: 60)));

  final String _api;
  final String _ws;
  final Dio _dio;

  String _d(String id) => '$_api/v1/drivers/${Uri.encodeComponent(id)}';

  @override
  Future<ForecastHeatmap> heatmap(int horizon) async => ForecastHeatmap.fromJson(
      _json((await _dio.get<dynamic>('$_api/v1/forecast/heatmap', queryParameters: {'horizon': horizon})).data));

  @override
  Future<TakeoverState> takeOver(String driverId) async =>
      TakeoverState.fromJson(_json((await _dio.post<dynamic>('${_d(driverId)}/takeover')).data));

  @override
  Future<void> release(String driverId) async => _dio.delete<dynamic>('${_d(driverId)}/takeover');

  @override
  Future<void> sendRide(String driverId) async => _dio.post<dynamic>('${_d(driverId)}/send-ride');

  @override
  Future<OfferResult> respond(String rideId, {required bool accept}) async => OfferResult.fromJson(_json((await _dio
          .post<dynamic>('$_api/v1/rides/${Uri.encodeComponent(rideId)}/offer/${accept ? 'accept' : 'decline'}'))
      .data));

  @override
  Future<RoutePath> route(double fromLat, double fromLng, double toLat, double toLng) async => RoutePath.fromJson(_json(
      (await _dio.get<dynamic>('$_api/v1/route', queryParameters: {
        'from_lat': fromLat,
        'from_lng': fromLng,
        'to_lat': toLat,
        'to_lng': toLng,
      }))
          .data));

  @override
  Stream<RideOffer> offers(String driverId) {
    final topic = 'driver:$driverId';
    late final StreamController<RideOffer> out;
    WebSocketChannel? ch;
    out = StreamController<RideOffer>(
      onListen: () {
        ch = WebSocketChannel.connect(Uri.parse('$_ws/v1/stream'));
        ch!.sink.add(jsonEncode({'action': 'subscribe', 'topics': [topic]}));
        ch!.stream.listen((raw) {
          final f = _json(raw);
          if (f['topic'] == topic) out.add(RideOffer.fromJson(_json(f['data'])));
        }, onError: (Object e) => out.addError(e), onDone: () => out.close());
      },
      onCancel: () => ch?.sink.close(),
    );
    return out.stream;
  }
}

/// Offline demo (USE_MOCK): a deterministic heatmap around the city centre;
/// "Send me a ride" offers a ride 600 m away; decline re-matches to d_017.
class MockTakeoverApi implements TakeoverApi {
  MockTakeoverApi({this.centerLat = 12.9716, this.centerLng = 77.5946});

  final double centerLat;
  final double centerLng;
  final _offers = StreamController<RideOffer>.broadcast();
  int _n = 0;

  List<(double, double)> _hex(double lat, double lng) => [
        for (var i = 0; i < 6; i++)
          (lat + 0.0042 * math.sin(i * math.pi / 3), lng + 0.0046 * math.cos(i * math.pi / 3)),
      ];

  @override
  Future<ForecastHeatmap> heatmap(int horizon) async {
    final cells = <ForecastCell>[];
    for (var i = 0; i < 18; i++) {
      final a = i * 0.9, r = 0.008 + 0.004 * (i % 5);
      final lat = centerLat + r * math.sin(a), lng = centerLng + r * math.cos(a);
      final rate = (4 + (i * 7 % 23)) * (1 + horizon / 60);
      cells.add(ForecastCell(
        h3: 'mock_$i',
        boundary: _hex(lat, lng),
        centerLat: lat,
        centerLng: lng,
        demandScore: rate / (rate + 12),
        requestsPerHr: rate,
        earningsPerHr: (165 * math.min(2.5, rate / 4)).round(),
        explanation: '~${rate.round()} rides/h expected ${horizon == 0 ? 'now' : 'in +$horizon min'} (mock data).',
      ));
    }
    cells.sort((a, b) => b.demandScore.compareTo(a.demandScore));
    return ForecastHeatmap(horizon: horizon, cells: cells, source: 'mock');
  }

  @override
  Future<TakeoverState> takeOver(String driverId) async =>
      TakeoverState(driverId: driverId, humanControlled: true, ttlS: 600);

  @override
  Future<void> release(String driverId) async {}

  @override
  Future<void> sendRide(String driverId) async {
    _n++;
    _offers.add(RideOffer(
      type: 'ride_offer', rideId: 'ride_mock_offer_$_n', driverId: driverId, riderId: 'r_demo_$_n',
      pickupLat: centerLat + 0.004, pickupLng: centerLng + 0.003, dropLat: centerLat + 0.03, dropLng: centerLng - 0.02,
      fareInr: 186, tripKm: 4.2, pickupEtaSec: 140, timeoutS: 15,
    ));
  }

  @override
  Future<OfferResult> respond(String rideId, {required bool accept}) async => accept
      ? OfferResult(rideId: rideId, decision: 'accepted', status: 'matched')
      : OfferResult(rideId: rideId, decision: 'declined', status: 'matched', rematchedTo: 'd_017');

  @override
  Future<RoutePath> route(double fromLat, double fromLng, double toLat, double toLng) async => RoutePath(
        points: [(fromLat, fromLng), (toLat, fromLng), (toLat, toLng)],
        distanceKm: haversineKm(fromLat, fromLng, toLat, toLng) * 1.3,
      );

  @override
  Stream<RideOffer> offers(String driverId) => _offers.stream.where((o) => o.driverId == driverId);
}
