import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../config/app_config.dart';
import '../data/safety_api.dart';
import '../voice/voice_io.dart';

/// Mock (offline demo) or the live gateway, like the Ops source.
final safetyApiProvider = Provider<SafetyApi>((ref) =>
    AppConfig.useMock ? MockSafetyApi() : DioSafetyApi(apiBaseUrl: AppConfig.apiBaseUrl));

/// A fresh voice engine per call screen.
final voiceProvider = Provider.autoDispose<VoiceIO>((ref) => PluginVoiceIO());
