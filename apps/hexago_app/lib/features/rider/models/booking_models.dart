/// Booking Intent agent payloads (POST /v1/agents/booking/*) and scheduled legs (GET /v1/scheduled).
library;

double _d(Object? v) => v is num ? v.toDouble() : double.tryParse('$v') ?? 0;
String _s(Object? v) => v == null ? '' : '$v';
String? _ns(Object? v) => v == null || '$v'.isEmpty ? null : '$v';
Map<String, dynamic> _m(Object? v) => v is Map ? Map<String, dynamic>.from(v) : <String, dynamic>{};
List<dynamic> _l(Object? v) => v is List ? v : const [];

class SavedPlace {
  const SavedPlace({required this.key, required this.label, required this.lat, required this.lng});
  final String key;
  final String label;
  final double lat;
  final double lng;

  factory SavedPlace.fromJson(Map<String, dynamic> j) =>
      SavedPlace(key: _s(j['key']), label: _s(j['label']), lat: _d(j['lat']), lng: _d(j['lng']));
}

class Persona {
  const Persona({required this.id, required this.name, this.blurb = '', this.kids = const [], this.places = const []});
  final String id;
  final String name;
  final String blurb;
  final List<String> kids;
  final List<SavedPlace> places;

  String get firstName => name.split(' ').first;

  factory Persona.fromJson(Map<String, dynamic> j) => Persona(
        id: _s(j['id']),
        name: _s(j['name']),
        blurb: _s(j['blurb']),
        kids: _l(j['kids']).map((e) => '$e').toList(),
        places: _l(j['saved_places']).map((e) => SavedPlace.fromJson(_m(e))).toList(),
      );
}

/// Which model answered, or "rule-based" for the deterministic fallback.
class AnsweredBy {
  const AnsweredBy({this.label = 'rule-based', this.model = '', this.usedFallback = true, this.fallbackReason});
  final String label;
  final String model;
  final bool usedFallback;
  final String? fallbackReason;

  factory AnsweredBy.fromJson(Map<String, dynamic> j) => AnsweredBy(
        label: _ns(j['label']) ?? 'rule-based',
        model: _s(j['model']),
        usedFallback: j['used_fallback'] == true,
        fallbackReason: _ns(j['fallback_reason']),
      );

  /// Short tag text, e.g. "gemini-3.5-flash-lite" or "rule-based (timeout)".
  String get tag => usedFallback && fallbackReason != null ? '$label ($fallbackReason)' : label;
}

class PlanPlace {
  const PlanPlace({required this.label, required this.lat, required this.lng, this.source = '', this.key = ''});
  final String label;
  final double lat;
  final double lng;
  final String source;
  final String key;

  /// "Office - Manyata Tech Park" -> "Office".
  String get short => label.split(' - ').first;

  factory PlanPlace.fromJson(Map<String, dynamic> j) => PlanPlace(
      label: _s(j['label']), lat: _d(j['lat']), lng: _d(j['lng']), source: _s(j['source']), key: _s(j['key']));
}

class FareBreakdown {
  const FareBreakdown({
    required this.total,
    this.base = 0,
    this.distanceFare = 0,
    this.timeFare = 0,
    this.surgeMultiplier = 1,
    this.currency = 'INR',
  });
  final double total;
  final double base;
  final double distanceFare;
  final double timeFare;
  final double surgeMultiplier;
  final String currency;

  bool get surging => surgeMultiplier > 1.0;

  factory FareBreakdown.fromJson(Map<String, dynamic> j) => FareBreakdown(
        total: _d(j['total']),
        base: _d(j['base']),
        distanceFare: _d(j['distance_fare']),
        timeFare: _d(j['time_fare']),
        surgeMultiplier: j['surge_multiplier'] == null ? 1 : _d(j['surge_multiplier']),
        currency: _ns(j['currency']) ?? 'INR',
      );
}

class PlanLeg {
  const PlanLeg({
    required this.seq,
    required this.passenger,
    required this.passengerName,
    required this.origin,
    required this.destination,
    required this.pickupTime,
    required this.tier,
    required this.price,
    this.tierLabel = '',
    this.arriveByTime,
    this.etaArrivalTime = '',
    this.durationMin = 0,
    this.distanceKm = 0,
    this.warnings = const [],
  });
  final int seq;
  final String passenger;
  final String passengerName;
  final PlanPlace origin;
  final PlanPlace destination;
  final String pickupTime;
  final String? arriveByTime;
  final String etaArrivalTime;
  final String tier;
  final String tierLabel;
  final FareBreakdown price;
  final double durationMin;
  final double distanceKm;
  final List<String> warnings;

  factory PlanLeg.fromJson(Map<String, dynamic> j) => PlanLeg(
        seq: _d(j['seq']).toInt(),
        passenger: _s(j['passenger']),
        passengerName: _s(j['passenger_name']),
        origin: PlanPlace.fromJson(_m(j['origin'])),
        destination: PlanPlace.fromJson(_m(j['destination'])),
        pickupTime: _s(j['pickup_time']),
        arriveByTime: _ns(j['arrive_by_time']),
        etaArrivalTime: _s(j['eta_arrival_time']),
        tier: _s(j['tier']),
        tierLabel: _s(j['tier_label']),
        price: FareBreakdown.fromJson(_m(j['price'])),
        durationMin: _d(j['duration_min']),
        distanceKm: _d(j['distance_km']),
        warnings: _l(j['warnings']).map((e) => '$e').toList(),
      );
}

/// What POST /v1/agents/booking/parse answers: a plan, or a clarifying question.
sealed class BookingReply {
  const BookingReply(this.answeredBy);
  final AnsweredBy answeredBy;

  static BookingReply fromJson(Map<String, dynamic> j) => j['type'] == 'question'
      ? ClarifyingQuestion.fromJson(j)
      : BookingPlan.fromJson(j);
}

class BookingPlan extends BookingReply {
  const BookingPlan({
    required this.planId,
    required this.legs,
    required this.totalFare,
    AnsweredBy answeredBy = const AnsweredBy(),
    this.warnings = const [],
    this.currency = 'INR',
    this.text = '',
  }) : super(answeredBy);
  final String planId;
  final List<PlanLeg> legs;
  final double totalFare;
  final List<String> warnings;
  final String currency;
  final String text;

  factory BookingPlan.fromJson(Map<String, dynamic> j) => BookingPlan(
        planId: _s(j['plan_id']),
        legs: _l(j['legs']).map((e) => PlanLeg.fromJson(_m(e))).toList()..sort((a, b) => a.seq.compareTo(b.seq)),
        totalFare: _d(j['total_fare']),
        warnings: _l(j['warnings']).map((e) => '$e').toList(),
        currency: _ns(j['currency']) ?? 'INR',
        text: _s(j['text']),
        answeredBy: AnsweredBy.fromJson(_m(j['answered_by'])),
      );
}

class ClarifyingQuestion extends BookingReply {
  const ClarifyingQuestion({
    required this.questionId,
    required this.question,
    this.options = const [],
    AnsweredBy answeredBy = const AnsweredBy(),
  }) : super(answeredBy);
  final String questionId;
  final String question;
  final List<String> options;

  factory ClarifyingQuestion.fromJson(Map<String, dynamic> j) => ClarifyingQuestion(
        questionId: _s(j['question_id']),
        question: _s(j['question']),
        options: _l(j['options']).map((e) => '$e').toList(),
        answeredBy: AnsweredBy.fromJson(_m(j['answered_by'])),
      );
}

class ConfirmResult {
  const ConfirmResult({required this.planId, required this.scheduledIds, this.demoMode = false});
  final String planId;
  final List<String> scheduledIds;
  final bool demoMode;

  factory ConfirmResult.fromJson(Map<String, dynamic> j) => ConfirmResult(
        planId: _s(j['plan_id']),
        scheduledIds: _l(j['scheduled_ride_ids']).map((e) => '$e').toList(),
        demoMode: j['demo_mode'] == true,
      );
}

/// A leg held by dispatch-go's scheduler.
class ScheduledLeg {
  const ScheduledLeg({
    required this.id,
    required this.planId,
    required this.seq,
    required this.status,
    required this.passengerName,
    required this.pickupName,
    required this.dropoffName,
    required this.tier,
    required this.pickupLat,
    required this.pickupLng,
    required this.dropoffLat,
    required this.dropoffLng,
    this.pickupAt,
    this.dispatchAt,
    this.rideId,
    this.rideStatus,
    this.driverId,
    this.fare = 0,
    this.note = '',
  });
  final String id;
  final String planId;
  final int seq;
  final String status; // scheduled | dispatched | completed | cancelled | failed
  final String passengerName;
  final String pickupName;
  final String dropoffName;
  final String tier;
  final double pickupLat;
  final double pickupLng;
  final double dropoffLat;
  final double dropoffLng;
  final DateTime? pickupAt;
  final DateTime? dispatchAt;
  final String? rideId;
  final String? rideStatus;
  final String? driverId;
  final double fare;
  final String note;

  bool get live => status == 'dispatched' && rideId != null;
  bool get finished => status == 'completed' || status == 'cancelled' || status == 'failed';

  factory ScheduledLeg.fromJson(Map<String, dynamic> j) {
    final p = _m(j['pickup']);
    final d = _m(j['dropoff']);
    return ScheduledLeg(
      id: _s(j['id']),
      planId: _s(j['plan_id']),
      seq: _d(j['seq']).toInt(),
      status: _s(j['status']),
      passengerName: _s(j['passenger_name']),
      pickupName: _s(j['pickup_name']),
      dropoffName: _s(j['dropoff_name']),
      tier: _s(j['tier']),
      pickupLat: _d(p['lat']),
      pickupLng: _d(p['lng']),
      dropoffLat: _d(d['lat']),
      dropoffLng: _d(d['lng']),
      pickupAt: DateTime.tryParse(_s(j['pickup_at'])),
      dispatchAt: DateTime.tryParse(_s(j['dispatch_at'])),
      rideId: _ns(j['ride_id']),
      rideStatus: _ns(j['ride_status']),
      driverId: _ns(j['driver_id']),
      fare: _d(j['fare']),
      note: _s(j['note']),
    );
  }
}

/// A `ride:<id>` stream frame.
class RideUpdate {
  const RideUpdate({required this.rideId, required this.status, this.driverLat, this.driverLng, this.etaSec = 0});
  final String rideId;
  final String status;
  final double? driverLat;
  final double? driverLng;
  final int etaSec;

  factory RideUpdate.fromJson(Map<String, dynamic> j) {
    final d = _m(j['driver']);
    return RideUpdate(
      rideId: _s(j['ride_id']),
      status: _s(j['status']),
      driverLat: d.containsKey('lat') ? _d(d['lat']) : null,
      driverLng: d.containsKey('lng') ? _d(d['lng']) : null,
      etaSec: _d(j['eta_sec']).toInt(),
    );
  }
}
