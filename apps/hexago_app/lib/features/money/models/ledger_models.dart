/// dispatch-go ledger views (amounts are integer paise, never doubles).
library;

int _int(Object? v) => v is int ? v : (v is num ? v.round() : int.tryParse('$v') ?? 0);
String _str(Object? v) => v?.toString() ?? '';
Map<String, dynamic> _map(Object? v) => v is Map ? Map<String, dynamic>.from(v) : <String, dynamic>{};
List<Map<String, dynamic>> _list(Object? v) => v is List ? [for (final e in v) _map(e)] : const [];

/// `123456` paise -> `₹1,234.56`.
String formatRupees(int paise) {
  final neg = paise < 0;
  final p = paise.abs();
  final rupees = (p ~/ 100).toString();
  final buf = StringBuffer();
  for (var i = 0; i < rupees.length; i++) {
    if (i > 0 && (rupees.length - i) % 3 == 0) buf.write(',');
    buf.write(rupees[i]);
  }
  return '${neg ? '-' : ''}₹$buf.${(p % 100).toString().padLeft(2, '0')}';
}

/// GET /v1/ledger/summary: the Ops "Money" tile.
class LedgerSummary {
  const LedgerSummary({
    required this.trialBalancePaise,
    required this.balanced,
    required this.payoutsSentCount,
    required this.payoutsSentPaise,
    required this.heldCount,
    required this.heldPaise,
    required this.platformRevenuePaise,
    required this.ridesSettled,
    this.store = '',
  });

  final int trialBalancePaise;
  final bool balanced;
  final int payoutsSentCount;
  final int payoutsSentPaise;
  final int heldCount;
  final int heldPaise;
  final int platformRevenuePaise;
  final int ridesSettled;
  final String store;

  factory LedgerSummary.fromJson(Map<String, dynamic> j) {
    final p = _map(j['payouts_today']);
    final tb = _int(j['trial_balance_paise']);
    return LedgerSummary(
      trialBalancePaise: tb,
      balanced: j['balanced'] == true && tb == 0,
      payoutsSentCount: _int(p['sent_count']),
      payoutsSentPaise: _int(p['sent_paise']),
      heldCount: _int(p['held_count']),
      heldPaise: _int(p['held_paise']),
      platformRevenuePaise: _int(j['platform_revenue_paise']),
      ridesSettled: _int(j['rides_settled']),
      store: _str(j['store']),
    );
  }
}

class LedgerPosting {
  const LedgerPosting({required this.account, required this.accountType, required this.owner, required this.amountPaise, required this.memo});

  final String account;
  final String accountType;
  final String owner;

  /// > 0 debit, < 0 credit.
  final int amountPaise;
  final String memo;

  bool get isDebit => amountPaise > 0;

  factory LedgerPosting.fromJson(Map<String, dynamic> j) => LedgerPosting(
        account: _str(j['account']),
        accountType: _str(j['account_type']),
        owner: _str(j['owner']),
        amountPaise: _int(j['amount_paise']),
        memo: _str(j['memo']),
      );
}

class LedgerEntry {
  const LedgerEntry({required this.id, required this.kind, required this.description, required this.postings, this.createdAt});

  final String id;
  final String kind; // ride_settlement | payout | refund | topup
  final String description;
  final List<LedgerPosting> postings;
  final DateTime? createdAt;

  int get sumPaise => postings.fold(0, (s, p) => s + p.amountPaise);

  factory LedgerEntry.fromJson(Map<String, dynamic> j) => LedgerEntry(
        id: _str(j['id']),
        kind: _str(j['kind']),
        description: _str(j['description']),
        postings: [for (final p in _list(j['postings'])) LedgerPosting.fromJson(p)],
        createdAt: DateTime.tryParse(_str(j['created_at'])),
      );
}

/// The per-ride fare split, role by role.
class FareBreakdown {
  const FareBreakdown({
    required this.farePaise,
    required this.promoPaise,
    required this.tipPaise,
    required this.riderPaidPaise,
    required this.payeeType,
    required this.payeeOwner,
    required this.payeePaise,
    required this.cityLevyPaise,
    required this.gstPaise,
    required this.platformFeePaise,
    this.refundedPaise = 0,
    this.payoutStatus = '',
    this.interrupted = false,
    this.incidentId = '',
  });

  final int farePaise;
  final int promoPaise;
  final int tipPaise;
  final int riderPaidPaise;
  final String payeeType; // driver_payable | fleet_operator_payable
  final String payeeOwner;
  final int payeePaise;
  final int cityLevyPaise;
  final int gstPaise;
  final int platformFeePaise;
  final int refundedPaise;
  final String payoutStatus; // pending | sent | held_for_review
  final bool interrupted;
  final String incidentId;

  bool get fleet => payeeType == 'fleet_operator_payable';

  factory FareBreakdown.fromJson(Map<String, dynamic> j) => FareBreakdown(
        farePaise: _int(j['fare_paise']),
        promoPaise: _int(j['promo_paise']),
        tipPaise: _int(j['tip_paise']),
        riderPaidPaise: _int(j['rider_paid_paise']),
        payeeType: _str(j['payee_type']),
        payeeOwner: _str(j['payee_owner']),
        payeePaise: _int(j['payee_paise']),
        cityLevyPaise: _int(j['city_levy_paise']),
        gstPaise: _int(j['gst_paise']),
        platformFeePaise: _int(j['platform_fee_paise']),
        refundedPaise: _int(j['refunded_paise']),
        payoutStatus: _str(j['payout_status']),
        interrupted: j['interrupted'] == true,
        incidentId: _str(j['incident_id']),
      );
}

/// GET /v1/ledger/entries?ride_id=
class RideLedger {
  const RideLedger({required this.rideId, required this.entries, this.breakdown});

  final String rideId;
  final List<LedgerEntry> entries;
  final FareBreakdown? breakdown;

  factory RideLedger.fromJson(Map<String, dynamic> j) {
    final b = j['breakdown'];
    return RideLedger(
      rideId: _str(j['ride_id']),
      entries: [for (final e in _list(j['entries'])) LedgerEntry.fromJson(e)],
      breakdown: b is Map ? FareBreakdown.fromJson(_map(b)) : null,
    );
  }
}

/// dispatch-go `ledger.ComputeSplit` (driver 75 %, fleet operator 80 %, levy 2 %,
/// GST 5 % on the platform fee, remainder to the platform). Used by the mock.
FareBreakdown computeSplit({required int farePaise, int promoPaise = 0, int tipPaise = 0, bool fleet = false, String payee = ''}) {
  final promo = promoPaise < farePaise ? promoPaise : farePaise;
  final share = farePaise * (fleet ? 8000 : 7500) ~/ 10000;
  final levy = farePaise * 200 ~/ 10000;
  final rest = farePaise - share - levy;
  final gst = rest * 500 ~/ 10500;
  return FareBreakdown(
    farePaise: farePaise,
    promoPaise: promo,
    tipPaise: tipPaise,
    riderPaidPaise: farePaise - promo + tipPaise,
    payeeType: fleet ? 'fleet_operator_payable' : 'driver_payable',
    payeeOwner: payee,
    payeePaise: share + tipPaise,
    cityLevyPaise: levy,
    gstPaise: gst,
    platformFeePaise: rest - gst,
  );
}
