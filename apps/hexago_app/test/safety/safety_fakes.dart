import 'package:hexago_app/features/safety/data/safety_api.dart';
import 'package:hexago_app/features/safety/models/safety_models.dart';
import 'package:hexago_app/features/safety/voice/voice_io.dart';

const sampleQuestion = 'Hi, this is HexaGo Safety. We noticed your vehicle left the planned route. Are you okay?';

SafetyCall sampleCall({String status = 'pending', double remaining = 15, String incidentStatus = 'check_in'}) => SafetyCall(
      checkinId: 'chk_1',
      incidentId: 'inc_1',
      rideId: 'ride-1234567',
      driverId: 'd_007',
      question: sampleQuestion,
      timeoutS: 15,
      remainingS: remaining,
      status: status,
      incidentStatus: incidentStatus,
    );

class FakeSafetyApi implements SafetyApi {
  FakeSafetyApi({SafetyCall? call, this.list, this.detail}) : call = call ?? sampleCall();

  SafetyCall call;
  IncidentList? list;
  IncidentDetail? detail;
  Object? loadError;
  Object? replyError;
  final replies = <String>[];
  int checkinLoads = 0;

  @override
  Future<SafetyCall> getCheckin(String id) async {
    checkinLoads++;
    if (loadError != null) throw loadError!;
    return call;
  }

  @override
  Future<ReplyResult> reply(String id, String transcript) async {
    replies.add(transcript);
    if (replyError != null) throw replyError!;
    final help = RegExp(r'help|scared|stop', caseSensitive: false).hasMatch(transcript);
    return ReplyResult(
      action: help ? 'escalate' : 'monitor',
      label: help ? 'distress' : 'fine',
      incidentStatus: help ? 'escalated' : 'monitoring',
      matched: [help ? 'help' : 'fine'],
    );
  }

  @override
  Future<IncidentList> listIncidents() async => list ?? const IncidentList();

  @override
  Future<IncidentDetail> getIncident(String id) async => detail!;
}

class FakeVoice implements VoiceIO {
  FakeVoice({this.speech = false});

  final bool speech;
  final spoken = <String>[];
  void Function(String, bool)? onWords;
  bool _listening = false;
  int stops = 0;

  @override
  Future<void> speak(String text) async => spoken.add(text);
  @override
  Future<void> stopSpeaking() async => stops++;
  @override
  Future<bool> initSpeech() async => speech;
  @override
  bool get canListen => speech;
  @override
  bool get listening => _listening;
  @override
  Future<void> listen(void Function(String words, bool isFinal) onWords) async {
    _listening = true;
    this.onWords = onWords;
  }

  @override
  Future<void> stopListening() async => _listening = false;
}

final sampleDetail = IncidentDetail(
  summary: const IncidentSummary(
    id: 'inc_abc123',
    status: 'escalated',
    severity: 0.8,
    rideId: 'ride-1',
    driverId: 'd_007',
    reason: 'no reply from the rider within 15 s',
    flags: IncidentFlags(driverFrozen: true, emergencyNotified: true, locationShared: true, contactNotified: false),
  ),
  reasoning: 'Vehicle swerved 450 m off route and stopped.',
  reasoningSource: 'llm',
  timeline: [
    TimelineEntry(step: 'signal_received', actor: 'safety_detector', summary: 'safety signal on ride', at: DateTime.utc(2026, 11, 18, 6, 30)),
    TimelineEntry(step: 'checkin_started', actor: 'safety_sentinel', summary: 'calling the rider', at: DateTime.utc(2026, 11, 18, 6, 30, 1)),
    TimelineEntry(step: 'checkin_timeout', actor: 'safety_sentinel', summary: 'no reply within 15 s: escalate'),
    const TimelineEntry(step: 'contact_notify_failed', actor: 'safety_sentinel', summary: 'contact notified failed', failed: true),
  ],
  transcript: const [
    TranscriptLine(role: 'agent', text: sampleQuestion),
    TranscriptLine(role: 'system', text: 'No answer within 15 s.'),
  ],
  plannedRoute: const [PathPoint(12.9716, 77.5946), PathPoint(12.9716, 77.6146)],
  vehiclePath: const [
    PathPoint(12.9716, 77.5946, speedKmh: 30),
    PathPoint(12.9716, 77.6000, speedKmh: 35),
    PathPoint(12.9760, 77.6030, speedKmh: 50),
    PathPoint(12.9770, 77.6040, speedKmh: 0),
  ],
  trackUrl: 'https://gw/track/tok',
  emergencyReference: 'EMS-1',
  replacementRideId: 'ride-2',
);
