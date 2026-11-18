import 'dart:async';
import 'dart:convert';

import 'package:dio/dio.dart';
import 'package:web_socket_channel/web_socket_channel.dart';

import '../models/booking_models.dart';

/// The Booking Intent agent and dispatch-go's scheduler, through the gateway.
abstract class BookingApi {
  Future<List<Persona>> personas();
  Future<BookingReply> parse(String riderId, String text, {String? replyTo, bool forceFallback = false});
  Future<ConfirmResult> confirm(String planId);
  Future<List<ScheduledLeg>> scheduled(String riderId);

  /// Live `ride:<id>` updates (driver position, status, ETA).
  Stream<RideUpdate> rideUpdates(String rideId);
}

Map<String, dynamic> _json(Object? data) {
  final d = data is String ? jsonDecode(data) : data;
  return d is Map ? Map<String, dynamic>.from(d) : <String, dynamic>{};
}

class DioBookingApi implements BookingApi {
  DioBookingApi({required String apiBaseUrl, required String wsBaseUrl, Dio? dio})
      : _api = _trim(apiBaseUrl),
        _ws = _trim(wsBaseUrl),
        // The LLM chain can take ~12 s; Render free instances can take ~60 s to wake.
        _dio = dio ??
            Dio(BaseOptions(connectTimeout: const Duration(seconds: 60), receiveTimeout: const Duration(seconds: 60)));

  static String _trim(String u) => u.endsWith('/') ? u.substring(0, u.length - 1) : u;
  final String _api;
  final String _ws;
  final Dio _dio;

  @override
  Future<List<Persona>> personas() async {
    final j = _json((await _dio.get<dynamic>('$_api/v1/agents/booking/personas')).data);
    return (j['personas'] as List? ?? const []).map((e) => Persona.fromJson(_json(e))).toList();
  }

  @override
  Future<BookingReply> parse(String riderId, String text, {String? replyTo, bool forceFallback = false}) async {
    final res = await _dio.post<dynamic>(
      '$_api/v1/agents/booking/parse',
      queryParameters: forceFallback ? {'force_fallback': 'true'} : null,
      data: {'rider_id': riderId, 'text': text, 'reply_to': ?replyTo},
    );
    return BookingReply.fromJson(_json(res.data));
  }

  @override
  Future<ConfirmResult> confirm(String planId) async =>
      ConfirmResult.fromJson(_json((await _dio.post<dynamic>('$_api/v1/agents/booking/confirm', data: {'plan_id': planId})).data));

  @override
  Future<List<ScheduledLeg>> scheduled(String riderId) async {
    final j = _json((await _dio.get<dynamic>('$_api/v1/scheduled', queryParameters: {'rider_id': riderId})).data);
    return (j['legs'] as List? ?? const []).map((e) => ScheduledLeg.fromJson(_json(e))).toList();
  }

  @override
  Stream<RideUpdate> rideUpdates(String rideId) {
    final topic = 'ride:$rideId';
    late final StreamController<RideUpdate> out;
    WebSocketChannel? ch;
    out = StreamController<RideUpdate>(
      onListen: () {
        ch = WebSocketChannel.connect(Uri.parse('$_ws/v1/stream'));
        ch!.sink.add(jsonEncode({'action': 'subscribe', 'topics': [topic]}));
        ch!.stream.listen((raw) {
          final f = _json(raw);
          if (f['topic'] == topic) out.add(RideUpdate.fromJson(_json(f['data'])));
        }, onError: (Object e) => out.addError(e), onDone: () => out.close());
      },
      onCancel: () => ch?.sink.close(),
    );
    return out.stream;
  }
}

/// Offline demo (USE_MOCK): the golden sentence and simple "from X to Y" requests.
class MockBookingApi implements BookingApi {
  MockBookingApi({DateTime Function()? now}) : _now = now ?? DateTime.now;
  final DateTime Function() _now;
  final _scheduled = <ScheduledLeg>[];
  int _plans = 0;

  static const priya = Persona(
    id: 'rider_priya',
    name: 'Priya Sharma',
    blurb: 'Parent in HSR Layout; works at Manyata Tech Park; daughter Anaya.',
    kids: ['Anaya'],
  );
  static const arjun = Persona(id: 'rider_arjun', name: 'Arjun Rao', blurb: 'Consultant in Indiranagar; two kids.');
  static const meera = Persona(id: 'rider_meera', name: 'Meera Iyer', blurb: 'Engineer in Jayanagar; son Rohan.');

  @override
  Future<List<Persona>> personas() async => const [priya, arjun, meera];

  static PlanLeg _leg(int seq, String who, String passenger, String from, String to, String pickup, String tier,
          double fare, {String? arriveBy}) =>
      PlanLeg(
        seq: seq,
        passenger: passenger,
        passengerName: who,
        origin: PlanPlace(label: from, lat: 12.93, lng: 77.62),
        destination: PlanPlace(label: to, lat: 12.97, lng: 77.6),
        pickupTime: pickup,
        arriveByTime: arriveBy,
        tier: tier,
        tierLabel: tier,
        price: FareBreakdown(total: fare),
      );

  @override
  Future<BookingReply> parse(String riderId, String text, {String? replyTo, bool forceFallback = false}) async {
    await Future<void>.delayed(const Duration(milliseconds: 600));
    final by = AnsweredBy(label: forceFallback ? 'rule-based' : 'mock-llm', usedFallback: forceFallback);
    final lower = text.toLowerCase();
    if (!lower.contains(' to ') && !lower.contains('drop') && replyTo == null) {
      return ClarifyingQuestion(
          questionId: 'q_mock', question: 'Where would you like to go?', options: const ['Home', 'Office', 'Airport'],
          answeredBy: by);
    }
    _plans++;
    final legs = lower.contains('soccer')
        ? [
            _leg(1, 'Anaya', 'kid', "Anaya's school", 'Soccer practice', '3:15 PM', 'mini', 152),
            _leg(2, 'Priya', 'me', 'Office', 'Kempegowda International Airport (BLR)', '5:01 PM', 'premium', 1015,
                arriveBy: '6:00 PM'),
          ]
        : [_leg(1, 'Priya', 'me', 'Current location', replyTo != null ? text : 'Destination', '${_now().hour}:00', 'mini', 180)];
    return BookingPlan(
        planId: 'pln_mock$_plans', legs: legs, totalFare: legs.fold(0, (a, l) => a + l.price.total), answeredBy: by,
        text: text);
  }

  @override
  Future<ConfirmResult> confirm(String planId) async {
    final now = _now();
    _scheduled
      ..clear()
      ..addAll([
        ScheduledLeg(id: 'sch_1', planId: planId, seq: 1, status: 'scheduled', passengerName: 'Anaya',
            pickupName: "Anaya's school", dropoffName: 'Soccer practice', tier: 'mini', pickupLat: 12.9352,
            pickupLng: 77.6265, dropoffLat: 12.969, dropoffLng: 77.6005, dispatchAt: now.add(const Duration(seconds: 20))),
        ScheduledLeg(id: 'sch_2', planId: planId, seq: 2, status: 'scheduled', passengerName: 'Priya',
            pickupName: 'Office', dropoffName: 'Airport', tier: 'premium', pickupLat: 13.0475, pickupLng: 77.62,
            dropoffLat: 13.1989, dropoffLng: 77.7068),
      ]);
    return ConfirmResult(planId: planId, scheduledIds: const ['sch_1', 'sch_2'], demoMode: true);
  }

  @override
  Future<List<ScheduledLeg>> scheduled(String riderId) async => List.of(_scheduled);

  @override
  Stream<RideUpdate> rideUpdates(String rideId) => const Stream.empty();
}
