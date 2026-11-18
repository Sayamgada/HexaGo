import 'package:flutter/foundation.dart';
import 'package:flutter_tts/flutter_tts.dart';
import 'package:speech_to_text/speech_recognition_result.dart';
import 'package:speech_to_text/speech_to_text.dart';

/// Speaks the agent's question and (where the platform supports it) listens
/// for the rider's answer. Every call is best-effort: a missing engine or a
/// denied microphone must never break the call screen, which always offers
/// a typed reply.
abstract class VoiceIO {
  Future<void> speak(String text);
  Future<void> stopSpeaking();

  /// Prepares speech recognition; false when unsupported or denied.
  Future<bool> initSpeech();
  bool get canListen;
  bool get listening;

  /// Streams recognised words; [isFinal] marks the end of the utterance.
  Future<void> listen(void Function(String words, bool isFinal) onWords);
  Future<void> stopListening();
}

/// flutter_tts + speech_to_text (Web Speech API on Chrome).
class PluginVoiceIO implements VoiceIO {
  PluginVoiceIO({FlutterTts? tts, SpeechToText? stt})
      : _tts = tts ?? FlutterTts(),
        _stt = stt ?? SpeechToText();

  final FlutterTts _tts;
  final SpeechToText _stt;
  bool _ready = false;
  bool _listening = false;

  @override
  Future<void> speak(String text) async {
    try {
      await _tts.setSpeechRate(kIsWeb ? 1.0 : 0.5);
      await _tts.speak(text);
    } catch (e) {
      debugPrint('safety call: text-to-speech unavailable: $e');
    }
  }

  @override
  Future<void> stopSpeaking() async {
    try {
      await _tts.stop();
    } catch (_) {}
  }

  @override
  Future<bool> initSpeech() async {
    try {
      _ready = await _stt.initialize(
        onError: (e) => debugPrint('safety call: speech error ${e.errorMsg}'),
        onStatus: (s) => _listening = s == 'listening',
      );
    } catch (e) {
      debugPrint('safety call: speech-to-text unavailable: $e');
      _ready = false;
    }
    return _ready;
  }

  @override
  bool get canListen => _ready;

  @override
  bool get listening => _listening;

  @override
  Future<void> listen(void Function(String words, bool isFinal) onWords) async {
    if (!_ready) return;
    try {
      _listening = true;
      await _stt.listen(
        onResult: (SpeechRecognitionResult r) => onWords(r.recognizedWords, r.finalResult),
        listenOptions: SpeechListenOptions(partialResults: true, listenFor: const Duration(seconds: 20)),
      );
    } catch (e) {
      _listening = false;
      debugPrint('safety call: listen failed: $e');
    }
  }

  @override
  Future<void> stopListening() async {
    _listening = false;
    try {
      await _stt.stop();
    } catch (_) {}
  }
}

/// No audio at all (tests, or platforms without engines): typed replies only.
class SilentVoiceIO implements VoiceIO {
  final spoken = <String>[];

  @override
  Future<void> speak(String text) async => spoken.add(text);
  @override
  Future<void> stopSpeaking() async {}
  @override
  Future<bool> initSpeech() async => false;
  @override
  bool get canListen => false;
  @override
  bool get listening => false;
  @override
  Future<void> listen(void Function(String words, bool isFinal) onWords) async {}
  @override
  Future<void> stopListening() async {}
}
