/// Driver module views: GET /v1/drivers, GET /v1/drivers/{id}, the driver's
/// wallet and payouts (amounts are integer paise).
library;

int _int(Object? v) => v is int ? v : (v is num ? v.round() : int.tryParse('$v') ?? 0);
double _num(Object? v) => v is num ? v.toDouble() : double.tryParse('$v') ?? 0;
double? _numOrNull(Object? v) => v == null ? null : _num(v);
String _str(Object? v) => v?.toString() ?? '';
Map<String, dynamic> _map(Object? v) => v is Map ? Map<String, dynamic>.from(v) : <String, dynamic>{};
List<Map<String, dynamic>> _list(Object? v) => v is List ? [for (final e in v) _map(e)] : const [];

/// The status chip: idle / to pickup / on trip / suspended (plus offline, incident).
enum DriverPhase {
  idle('Idle'),
  toPickup('To pickup'),
  onTrip('On trip'),
  suspended('Suspended'),
  incident('Incident'),
  offline('Offline');

  const DriverPhase(this.label);
  final String label;

  static DriverPhase fromWire(String s, {bool suspended = false}) {
    if (suspended) return DriverPhase.suspended;
    switch (s) {
      case 'en_route':
      case 'to_pickup':
        return DriverPhase.toPickup;
      case 'on_trip':
        return DriverPhase.onTrip;
      case 'suspended':
        return DriverPhase.suspended;
      case 'incident':
        return DriverPhase.incident;
      case 'offline':
        return DriverPhase.offline;
    }
    return DriverPhase.idle;
  }
}

const driverTiers = ['auto', 'mini', 'sedan', 'premium', 'xl', 'av'];

String tierLabel(String t) => switch (t) {
      'av' => 'AV',
      'xl' => 'XL',
      '' => '',
      _ => '${t[0].toUpperCase()}${t.substring(1)}',
    };

class DriverSummary {
  const DriverSummary({
    required this.id,
    required this.tier,
    required this.status,
    this.av = false,
    this.rating = 0,
    this.batteryPct,
    this.lat = 0,
    this.lng = 0,
    this.rideId = '',
    this.suspended = false,
    this.incidentId = '',
  });

  final String id;
  final String tier;
  final String status;
  final bool av;
  final double rating;
  final double? batteryPct;
  final double lat;
  final double lng;
  final String rideId;
  final bool suspended;
  final String incidentId;

  DriverPhase get phase => DriverPhase.fromWire(status, suspended: suspended);

  factory DriverSummary.fromJson(Map<String, dynamic> j) => DriverSummary(
        id: _str(j['id']),
        tier: _str(j['tier']),
        status: _str(j['status']),
        av: j['av'] == true || _str(j['tier']) == 'av',
        rating: _num(j['rating']),
        batteryPct: _numOrNull(j['battery_pct']),
        lat: _num(j['lat']),
        lng: _num(j['lng']),
        rideId: _str(j['ride_id']),
        suspended: j['suspended'] == true,
        incidentId: _str(j['incident_id']),
      );
}

class CurrentRide {
  const CurrentRide({
    required this.id,
    required this.riderId,
    required this.status,
    required this.pickupLat,
    required this.pickupLng,
    required this.dropLat,
    required this.dropLng,
    this.etaSec = 0,
    this.progress = 0,
  });

  final String id;
  final String riderId;
  final String status; // accepted | arriving | on_trip
  final double pickupLat, pickupLng, dropLat, dropLng;
  final int etaSec;
  final double progress; // 0..1 along the current leg

  bool get onBoard => status == 'on_trip';
  String get legLabel => onBoard ? 'to drop-off' : 'to pickup';

  factory CurrentRide.fromJson(Map<String, dynamic> j) {
    final p = _map(j['pickup']), d = _map(j['dropoff']);
    return CurrentRide(
      id: _str(j['id']),
      riderId: _str(j['rider_id']),
      status: _str(j['status']),
      pickupLat: _num(p['lat']),
      pickupLng: _num(p['lng']),
      dropLat: _num(d['lat']),
      dropLng: _num(d['lng']),
      etaSec: _int(j['eta_sec']),
      progress: _num(j['progress']).clamp(0, 1).toDouble(),
    );
  }
}

class AvStatus {
  const AvStatus({required this.batteryPct, required this.health, required this.healthNote, required this.mode});

  final double batteryPct;
  final String health; // ok | service_due | low_battery
  final String healthNote;
  final String mode;

  String get modeLabel => switch (mode) {
        'autonomous_to_pickup' => 'Autonomous: driving to pickup',
        'autonomous_on_trip' => 'Autonomous: passenger on board',
        'repositioning' => 'Autonomous: repositioning',
        'charging' => 'Charging',
        'minimal_risk_stop' => 'Minimal-risk stop',
        'remote_hold' => 'Remote hold (Safety)',
        'parked' => 'Parked',
        _ => mode,
      };

  factory AvStatus.fromJson(Map<String, dynamic> j) => AvStatus(
        batteryPct: _num(j['battery_pct']),
        health: _str(j['health']),
        healthNote: _str(j['health_note']),
        mode: _str(j['mode']),
      );
}

class DriverDetail {
  const DriverDetail({
    required this.summary,
    this.heading = 0,
    this.speedKmh = 0,
    this.ride,
    this.avStatus,
    this.suspendedAt,
    this.suspensionReason = '',
  });

  final DriverSummary summary;
  final double heading;
  final double speedKmh;
  final CurrentRide? ride;
  final AvStatus? avStatus;
  final DateTime? suspendedAt;
  final String suspensionReason;

  String get id => summary.id;
  bool get suspended => summary.suspended;
  String get incidentId => summary.incidentId;
  DriverPhase get phase => summary.phase;

  factory DriverDetail.fromJson(Map<String, dynamic> j) {
    final sp = _map(j['suspension']);
    final ride = j['ride'], av = j['av_status'];
    return DriverDetail(
      summary: DriverSummary.fromJson(j),
      heading: _num(j['heading']),
      speedKmh: _num(j['speed_kmh']),
      ride: ride is Map ? CurrentRide.fromJson(_map(ride)) : null,
      avStatus: av is Map ? AvStatus.fromJson(_map(av)) : null,
      suspendedAt: DateTime.tryParse(_str(sp['suspended_at'])),
      suspensionReason: _str(sp['reason']),
    );
  }
}

/// One ledger payout (GET /v1/ledger/payouts?driver_id=).
class DriverPayout {
  const DriverPayout({
    required this.id,
    required this.rideId,
    required this.owner,
    required this.amountPaise,
    required this.status,
    this.incidentId = '',
    this.createdAt,
  });

  final String id;
  final String rideId;
  final String owner;
  final int amountPaise;
  final String status; // pending | sent | held_for_review | released
  final String incidentId;
  final DateTime? createdAt;

  bool get held => status == 'held_for_review';

  String get statusLabel => switch (status) {
        'sent' => 'Paid',
        'held_for_review' => 'Held for review',
        'pending' => 'Pending',
        _ => status.isEmpty ? 'Pending' : '${status[0].toUpperCase()}${status.substring(1).replaceAll('_', ' ')}',
      };

  factory DriverPayout.fromJson(Map<String, dynamic> j) => DriverPayout(
        id: _str(j['id']),
        rideId: _str(j['ride_id']),
        owner: _str(j['owner']),
        amountPaise: _int(j['amount_paise']),
        status: _str(j['status']),
        incidentId: _str(j['incident_id']),
        createdAt: DateTime.tryParse(_str(j['created_at'])),
      );
}

/// GET /v1/wallets/{owner}.
class DriverWallet {
  const DriverWallet({required this.owner, required this.balancePaise, this.paidOutPaise = 0, this.heldPaise = 0});

  final String owner;
  final int balancePaise;
  final int paidOutPaise;
  final int heldPaise;

  factory DriverWallet.fromJson(Map<String, dynamic> j) => DriverWallet(
        owner: _str(j['owner']),
        balancePaise: _int(j['balance_paise']),
        paidOutPaise: _int(j['paid_out_paise']),
        heldPaise: _int(j['held_paise']),
      );
}

List<DriverPayout> payoutsFromJson(Map<String, dynamic> j) =>
    [for (final p in _list(j['payouts'])) DriverPayout.fromJson(p)];

List<DriverSummary> driversFromJson(Map<String, dynamic> j) =>
    [for (final d in _list(j['drivers'])) DriverSummary.fromJson(d)];

/// Midnight IST (UTC+05:30) of [now], in UTC.
DateTime startOfDayIst(DateTime now) {
  final ist = now.toUtc().add(const Duration(hours: 5, minutes: 30));
  return DateTime.utc(ist.year, ist.month, ist.day).subtract(const Duration(hours: 5, minutes: 30));
}

/// Today's earnings from the payout list.
class EarningsToday {
  const EarningsToday({required this.trips, required this.totalPaise, required this.paidPaise, required this.heldPaise, required this.payouts});

  final int trips;
  final int totalPaise;
  final int paidPaise;
  final int heldPaise;
  final List<DriverPayout> payouts; // today's, newest first

  factory EarningsToday.from(List<DriverPayout> all, DateTime now) {
    final since = startOfDayIst(now);
    final today = [for (final p in all) if (p.createdAt == null || !p.createdAt!.isBefore(since)) p]
      ..sort((a, b) => (b.createdAt ?? now).compareTo(a.createdAt ?? now));
    var total = 0, paid = 0, held = 0;
    for (final p in today) {
      total += p.amountPaise;
      if (p.held) {
        held += p.amountPaise;
      } else if (p.status == 'sent' || p.status == 'released') {
        paid += p.amountPaise;
      }
    }
    return EarningsToday(
      trips: {for (final p in today) p.rideId}.length,
      totalPaise: total,
      paidPaise: paid,
      heldPaise: held,
      payouts: today,
    );
  }
}

/// `754` -> `12 min`, `40` -> `1 min`, `0` -> `now`.
String formatEta(int sec) {
  if (sec <= 0) return 'now';
  final m = (sec / 60).ceil();
  return m >= 60 ? '${m ~/ 60} h ${m % 60} min' : '$m min';
}
