import 'dart:async';

import 'package:hexago_app/features/rider/data/booking_api.dart';
import 'package:hexago_app/features/rider/models/booking_models.dart';

const priya = Persona(id: 'rider_priya', name: 'Priya Sharma', blurb: 'Parent in HSR Layout', kids: ['Anaya']);
const arjun = Persona(id: 'rider_arjun', name: 'Arjun Rao', blurb: 'Two kids');

const goldenSentence = 'Pick up my kid from school at 3:15 PM, drop them at soccer practice, and then book a '
    'high-tier sedan for me from my office to the airport to arrive by 6:00 PM.';

/// The agent's JSON for the golden sentence (shape of POST /v1/agents/booking/parse).
Map<String, dynamic> goldenPlanJson({bool fallback = false}) => {
      'type': 'plan',
      'plan_id': 'pln_golden',
      'text': goldenSentence,
      'total_fare': 1167,
      'currency': 'INR',
      'warnings': <String>[],
      'answered_by': fallback
          ? {'label': 'rule-based', 'model': 'rule-based', 'used_fallback': true, 'fallback_reason': 'forced'}
          : {'label': 'gemini-3.5-flash-lite', 'model': 'gemini/gemini-3.5-flash-lite', 'used_fallback': false},
      'legs': [
        {
          'seq': 2,
          'passenger': 'me',
          'passenger_name': 'Priya',
          'origin': {'label': 'Office - Manyata Tech Park, Hebbal', 'lat': 13.0475, 'lng': 77.62, 'source': 'saved'},
          'destination': {'label': 'Kempegowda International Airport (BLR)', 'lat': 13.1989, 'lng': 77.7068},
          'pickup_time': '5:01 PM',
          'arrive_by_time': '6:00 PM',
          'eta_arrival_time': '5:50 PM',
          'tier': 'premium',
          'tier_label': 'Premium sedan',
          'duration_min': 48.5,
          'price': {'total': 1015, 'surge_multiplier': 1.0, 'base': 120},
          'warnings': ['Soccer -> Office ends 5:10 PM, Office -> Airport needs 55 min and leaves at 5:05 PM - overlap.'],
        },
        {
          'seq': 1,
          'passenger': 'kid',
          'passenger_name': 'Anaya',
          'origin': {'label': "Anaya's school - National Public School, Koramangala", 'lat': 12.9352, 'lng': 77.6265},
          'destination': {'label': 'Soccer practice - Bangalore Football Stadium', 'lat': 12.969, 'lng': 77.6005},
          'pickup_time': '3:15 PM',
          'eta_arrival_time': '3:36 PM',
          'tier': 'mini',
          'tier_label': 'Mini',
          'duration_min': 21,
          'price': {'total': 152, 'surge_multiplier': 1.5},
          'warnings': <String>[],
        },
      ],
    };

class FakeBookingApi implements BookingApi {
  FakeBookingApi({this.replies});

  /// Replies to hand out in order (default: the golden plan for anything).
  List<BookingReply>? replies;
  final parses = <({String rider, String text, String? replyTo, bool force})>[];
  final confirms = <String>[];
  List<ScheduledLeg> scheduledLegs = const [];
  Object? parseError;
  Object? confirmError;
  Completer<void>? holdParse;
  final rideStreams = <String, StreamController<RideUpdate>>{};

  @override
  Future<List<Persona>> personas() async => const [priya, arjun];

  @override
  Future<BookingReply> parse(String riderId, String text, {String? replyTo, bool forceFallback = false}) async {
    parses.add((rider: riderId, text: text, replyTo: replyTo, force: forceFallback));
    if (holdParse != null) await holdParse!.future;
    if (parseError != null) throw parseError!;
    final r = replies;
    if (r != null && r.isNotEmpty) return r.removeAt(0);
    return BookingPlan.fromJson(goldenPlanJson(fallback: forceFallback));
  }

  @override
  Future<ConfirmResult> confirm(String planId) async {
    confirms.add(planId);
    if (confirmError != null) throw confirmError!;
    return ConfirmResult(planId: planId, scheduledIds: const ['sch_1', 'sch_2'], demoMode: true);
  }

  @override
  Future<List<ScheduledLeg>> scheduled(String riderId) async => scheduledLegs;

  @override
  Stream<RideUpdate> rideUpdates(String rideId) =>
      rideStreams.putIfAbsent(rideId, () => StreamController<RideUpdate>.broadcast()).stream;
}

ScheduledLeg sched(int seq, String status,
        {String? rideId, String? rideStatus, DateTime? dispatchAt, String planId = 'pln_golden', String note = ''}) =>
    ScheduledLeg(
      id: 'sch_$seq',
      planId: planId,
      seq: seq,
      status: status,
      passengerName: seq == 1 ? 'Anaya' : 'Priya',
      pickupName: seq == 1 ? "Anaya's school - NPS" : 'Office - Manyata',
      dropoffName: seq == 1 ? 'Soccer practice - Stadium' : 'Airport',
      tier: seq == 1 ? 'mini' : 'premium',
      pickupLat: 12.93,
      pickupLng: 77.62,
      dropoffLat: 12.97,
      dropoffLng: 77.6,
      dispatchAt: dispatchAt,
      rideId: rideId,
      rideStatus: rideStatus,
      driverId: rideId == null ? null : 'd_042',
      note: note,
    );
