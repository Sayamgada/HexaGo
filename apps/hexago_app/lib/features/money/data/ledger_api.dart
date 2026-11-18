import 'dart:convert';

import 'package:dio/dio.dart';

import '../models/ledger_models.dart';

/// dispatch-go ledger endpoints (through the gateway).
abstract class LedgerApi {
  Future<LedgerSummary> summary();
  Future<RideLedger> ride(String rideId);
}

Map<String, dynamic> _json(Object? data) {
  final d = data is String ? jsonDecode(data) : data;
  return d is Map ? Map<String, dynamic>.from(d) : <String, dynamic>{};
}

class DioLedgerApi implements LedgerApi {
  DioLedgerApi({required String apiBaseUrl, Dio? dio})
      : _api = apiBaseUrl.endsWith('/') ? apiBaseUrl.substring(0, apiBaseUrl.length - 1) : apiBaseUrl,
        _dio = dio ??
            Dio(BaseOptions(connectTimeout: const Duration(seconds: 60), receiveTimeout: const Duration(seconds: 60)));

  final String _api;
  final Dio _dio;

  @override
  Future<LedgerSummary> summary() async =>
      LedgerSummary.fromJson(_json((await _dio.get<dynamic>('$_api/v1/ledger/summary')).data));

  @override
  Future<RideLedger> ride(String rideId) async => RideLedger.fromJson(_json(
      (await _dio.get<dynamic>('$_api/v1/ledger/entries', queryParameters: {'ride_id': rideId})).data));
}

/// Offline demo ledger (USE_MOCK): a growing, always-balanced book and
/// deterministic per-ride splits computed with the real formula.
class MockLedgerApi implements LedgerApi {
  MockLedgerApi({DateTime Function()? now}) : _now = now ?? DateTime.now {
    _start = _now();
  }

  final DateTime Function() _now;
  late final DateTime _start;

  @override
  Future<LedgerSummary> summary() async {
    final rides = 40 + _now().difference(_start).inSeconds ~/ 3;
    final perRide = computeSplit(farePaise: 21000);
    return LedgerSummary(
      trialBalancePaise: 0,
      balanced: true,
      payoutsSentCount: rides - 1,
      payoutsSentPaise: (rides - 1) * perRide.payeePaise,
      heldCount: 1,
      heldPaise: perRide.payeePaise,
      platformRevenuePaise: rides * perRide.platformFeePaise,
      ridesSettled: rides,
      store: 'mock',
    );
  }

  @override
  Future<RideLedger> ride(String rideId) async {
    final h = rideId.codeUnits.fold<int>(7, (a, c) => (a * 31 + c) & 0x7fffffff);
    final fleet = h % 8 == 0;
    final b = computeSplit(
      farePaise: (8000 + h % 40000) ~/ 100 * 100,
      promoPaise: h % 10 == 0 ? 2500 : 0,
      tipPaise: h % 7 == 0 ? 2000 : 0,
      fleet: fleet,
      payee: fleet ? 'fleet_op_blr' : 'd_${(h % 300).toString().padLeft(3, '0')}',
    );
    final payee = '${b.payeeType}:${b.payeeOwner}';
    LedgerPosting post(String account, int amount, String memo) => LedgerPosting(
        account: account, accountType: account.split(':').first, owner: account.split(':').last, amountPaise: amount, memo: memo);
    final postings = [
      post('payment_clearing:gateway', b.riderPaidPaise, 'card'),
      if (b.promoPaise > 0) post('promo_wallet:hexago', b.promoPaise, 'promo'),
      post(payee, -(b.payeePaise - b.tipPaise), 'payee_share'),
      if (b.tipPaise > 0) post(payee, -b.tipPaise, 'tip'),
      post('city_levy_payable:bengaluru', -b.cityLevyPaise, 'city_levy'),
      post('tax_payable:gst', -b.gstPaise, 'gst'),
      post('platform_revenue:hexago', -b.platformFeePaise, 'platform_fee'),
    ];
    return RideLedger(
      rideId: rideId,
      breakdown: FareBreakdown(
        farePaise: b.farePaise, promoPaise: b.promoPaise, tipPaise: b.tipPaise, riderPaidPaise: b.riderPaidPaise,
        payeeType: b.payeeType, payeeOwner: b.payeeOwner, payeePaise: b.payeePaise, cityLevyPaise: b.cityLevyPaise,
        gstPaise: b.gstPaise, platformFeePaise: b.platformFeePaise, payoutStatus: 'sent',
      ),
      entries: [
        LedgerEntry(id: 'je_mock_$h', kind: 'ride_settlement', description: 'ride $rideId', postings: postings),
        LedgerEntry(id: 'je_mock_po_$h', kind: 'payout', description: 'instant payout', postings: [
          post(payee, b.payeePaise, 'payout'),
          post('payment_clearing:gateway', -b.payeePaise, 'payout'),
        ]),
      ],
    );
  }
}
