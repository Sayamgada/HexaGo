import 'dart:convert';
import 'dart:math' as math;

import 'package:dio/dio.dart';

import '../../money/models/ledger_models.dart' show computeSplit;
import '../models/driver_models.dart';

/// The Driver module's gateway calls.
abstract class DriverApi {
  Future<List<DriverSummary>> drivers({String? tier, String? query});
  Future<DriverDetail> driver(String id);
  Future<DriverWallet> wallet(String owner);
  Future<List<DriverPayout>> payouts(String driverId);
}

Map<String, dynamic> _json(Object? data) {
  final d = data is String ? jsonDecode(data) : data;
  return d is Map ? Map<String, dynamic>.from(d) : <String, dynamic>{};
}

class DioDriverApi implements DriverApi {
  DioDriverApi({required String apiBaseUrl, Dio? dio})
      : _api = apiBaseUrl.endsWith('/') ? apiBaseUrl.substring(0, apiBaseUrl.length - 1) : apiBaseUrl,
        _dio = dio ??
            Dio(BaseOptions(connectTimeout: const Duration(seconds: 60), receiveTimeout: const Duration(seconds: 60)));

  final String _api;
  final Dio _dio;

  @override
  Future<List<DriverSummary>> drivers({String? tier, String? query}) async {
    final r = await _dio.get<dynamic>('$_api/v1/drivers', queryParameters: {
      if (tier != null && tier.isNotEmpty) 'tier': tier,
      if (query != null && query.trim().isNotEmpty) 'q': query.trim(),
    });
    return driversFromJson(_json(r.data));
  }

  @override
  Future<DriverDetail> driver(String id) async =>
      DriverDetail.fromJson(_json((await _dio.get<dynamic>('$_api/v1/drivers/${Uri.encodeComponent(id)}')).data));

  @override
  Future<DriverWallet> wallet(String owner) async =>
      DriverWallet.fromJson(_json((await _dio.get<dynamic>('$_api/v1/wallets/${Uri.encodeComponent(owner)}')).data));

  @override
  Future<List<DriverPayout>> payouts(String driverId) async => payoutsFromJson(_json(
      (await _dio.get<dynamic>('$_api/v1/ledger/payouts', queryParameters: {'driver_id': driverId, 'limit': 100})).data));
}

/// Offline demo (USE_MOCK): 40 drivers around Bengaluru; every third one is
/// on a trip that loops along a straight line, d_007 is suspended.
class MockDriverApi implements DriverApi {
  MockDriverApi({this.centerLat = 12.9716, this.centerLng = 77.5946, DateTime Function()? now}) : _now = now ?? DateTime.now;

  final double centerLat;
  final double centerLng;
  final DateTime Function() _now;

  static const _count = 40;

  String _id(int n) => 'd_${n.toString().padLeft(3, '0')}';
  String _tier(int n) => n % 7 == 0 ? 'av' : driverTiers[n % 5];

  DriverDetail _detail(int n) {
    final tier = _tier(n);
    final av = tier == 'av';
    final suspended = n == 7;
    final busy = !suspended && n % 3 == 0;
    final a = n * 0.7;
    final pickup = (centerLat + 0.03 * math.sin(a), centerLng + 0.03 * math.cos(a));
    final drop = (pickup.$1 + 0.025, pickup.$2 - 0.02);
    final t = (_now().millisecondsSinceEpoch / 1000 + n * 11) % 240 / 240; // 4-minute loop
    final onBoard = t >= 0.4;
    final leg = onBoard ? (t - 0.4) / 0.6 : t / 0.4;
    final from = onBoard ? pickup : (pickup.$1 - 0.01, pickup.$2 - 0.01);
    final to = onBoard ? drop : pickup;
    final pos = busy ? (from.$1 + (to.$1 - from.$1) * leg, from.$2 + (to.$2 - from.$2) * leg) : pickup;
    final battery = av ? 35 + (n * 13 % 60).toDouble() : null;
    final status = suspended ? 'suspended' : (busy ? (onBoard ? 'on_trip' : 'en_route') : 'idle');
    return DriverDetail(
      summary: DriverSummary(
        id: _id(n), tier: tier, status: status, av: av, rating: 4.3 + (n % 7) / 10, batteryPct: battery,
        lat: pos.$1, lng: pos.$2, rideId: busy ? 'ride_mock_$n' : '', suspended: suspended,
        incidentId: suspended ? 'inc_mock_7' : '',
      ),
      heading: busy ? 315 : 0,
      speedKmh: busy ? 28 : 0,
      ride: busy
          ? CurrentRide(
              id: 'ride_mock_$n', riderId: 'r_${(n * 17 % 200).toString().padLeft(3, '0')}',
              status: onBoard ? 'on_trip' : 'arriving', pickupLat: pickup.$1, pickupLng: pickup.$2,
              dropLat: drop.$1, dropLng: drop.$2, etaSec: ((1 - leg) * (onBoard ? 144 : 96)).round(), progress: leg)
          : null,
      avStatus: av
          ? AvStatus(
              batteryPct: battery!, health: 'ok', healthNote: 'All systems nominal: lidar, brakes, tyres, drivetrain',
              mode: suspended ? 'remote_hold' : (busy ? (onBoard ? 'autonomous_on_trip' : 'autonomous_to_pickup') : 'repositioning'))
          : null,
      suspensionReason: suspended ? 'route deviation with unanswered check-in' : '',
      suspendedAt: suspended ? _now().subtract(const Duration(minutes: 4)) : null,
    );
  }

  @override
  Future<List<DriverSummary>> drivers({String? tier, String? query}) async => [
        for (var n = 1; n <= _count; n++)
          if ((tier == null || tier.isEmpty || _tier(n) == tier) && (query == null || _id(n).contains(query.trim().toLowerCase())))
            _detail(n).summary,
      ];

  int _n(String id) => int.tryParse(id.replaceFirst('d_', '')) ?? 0;

  @override
  Future<DriverDetail> driver(String id) async {
    final n = _n(id);
    if (n < 1 || n > _count) throw StateError('driver not found: $id');
    return _detail(n);
  }

  @override
  Future<List<DriverPayout>> payouts(String driverId) async {
    final n = _n(driverId);
    final av = _tier(n) == 'av';
    final now = _now();
    return [
      for (var i = 0; i < 4 + n % 4; i++)
        DriverPayout(
          id: 'po_mock_${n}_$i',
          rideId: 'ride_mock_${n}_$i',
          owner: av ? 'fleet_op_blr' : driverId,
          amountPaise: computeSplit(farePaise: 12000 + (n * 37 + i * 53) % 30 * 1000, fleet: av).payeePaise,
          status: n == 7 && i == 0 ? 'held_for_review' : 'sent',
          incidentId: n == 7 && i == 0 ? 'inc_mock_7' : '',
          createdAt: now.subtract(Duration(minutes: 5 + i * 37)),
        ),
    ];
  }

  @override
  Future<DriverWallet> wallet(String owner) async {
    final ps = await payouts(owner.startsWith('d_') ? owner : 'd_007');
    final held = ps.where((p) => p.held).fold(0, (s, p) => s + p.amountPaise);
    final paid = ps.where((p) => !p.held).fold(0, (s, p) => s + p.amountPaise);
    return DriverWallet(owner: owner, balancePaise: held, paidOutPaise: paid, heldPaise: held);
  }
}
