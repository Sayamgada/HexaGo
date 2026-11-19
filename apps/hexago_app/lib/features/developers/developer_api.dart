import 'dart:convert';

import 'package:dio/dio.dart';

/// The API scopes a key can carry (dispatch-go `internal/developer`).
const apiScopes = <String, String>{
  'rides:read': 'Rides, ETAs, supply, incidents',
  'rides:write': 'Request / cancel rides, refunds',
  'pricing:read': 'Quotes, rate card, surge map',
  'stream:read': 'Live WebSocket stream',
  'agents:invoke': 'Booking agent and the agent swarm',
  'ledger:read': 'Wallets, entries, payouts',
};

/// A key as returned once by POST /v1/developer/keys.
class CreatedKey {
  const CreatedKey({
    required this.apiKey,
    required this.id,
    required this.prefix,
    required this.scopes,
    required this.rateLimitPerMin,
  });

  factory CreatedKey.fromJson(Map<String, dynamic> j) => CreatedKey(
        apiKey: j['api_key'] as String? ?? '',
        id: j['id'] as String? ?? '',
        prefix: j['prefix'] as String? ?? '',
        scopes: [for (final s in (j['scopes'] as List? ?? const [])) s.toString()],
        rateLimitPerMin: (j['rate_limit_per_min'] as num?)?.toInt() ?? 60,
      );

  final String apiKey;
  final String id;
  final String prefix;
  final List<String> scopes;
  final int rateLimitPerMin;
}

/// The self-serve developer endpoints of the gateway.
abstract class DeveloperApi {
  String get baseUrl;
  Future<CreatedKey> createKey({required String name, required List<String> scopes});
}

class DioDeveloperApi implements DeveloperApi {
  DioDeveloperApi({required String apiBaseUrl, Dio? dio})
      : baseUrl = apiBaseUrl.endsWith('/') ? apiBaseUrl.substring(0, apiBaseUrl.length - 1) : apiBaseUrl,
        _dio = dio ??
            Dio(BaseOptions(connectTimeout: const Duration(seconds: 60), receiveTimeout: const Duration(seconds: 60)));

  @override
  final String baseUrl;
  final Dio _dio;

  @override
  Future<CreatedKey> createKey({required String name, required List<String> scopes}) async {
    try {
      final res = await _dio.post<dynamic>('$baseUrl/v1/developer/keys', data: {'name': name, 'scopes': scopes});
      final data = res.data is String ? jsonDecode(res.data as String) : res.data;
      return CreatedKey.fromJson(Map<String, dynamic>.from(data as Map));
    } on DioException catch (e) {
      final body = e.response?.data;
      final msg = body is Map && body['error'] != null ? body['error'].toString() : (e.message ?? 'request failed');
      throw DeveloperApiException(e.response?.statusCode, msg);
    }
  }
}

class DeveloperApiException implements Exception {
  DeveloperApiException(this.status, this.message);
  final int? status;
  final String message;

  @override
  String toString() => status == null ? message : 'HTTP $status: $message';
}

/// Ready-to-paste curl examples for [baseUrl] and [key] (a shell variable when null).
List<({String title, String command})> curlExamples(String baseUrl, String? key) {
  final k = key ?? r'$HEXAGO_KEY';
  final auth = "-H 'Authorization: Bearer $k'";
  final ws = baseUrl.replaceFirst(RegExp('^http'), 'ws');
  return [
    (
      title: 'Request a ride',
      command: "curl -s -X POST $baseUrl/v1/rides $auth -H 'Content-Type: application/json' "
          "-d '{\"rider_id\":\"rider_demo\",\"pickup\":{\"lat\":12.9716,\"lng\":77.5946},"
          "\"dropoff\":{\"lat\":12.9352,\"lng\":77.6245},\"tier\":\"mini\"}'",
    ),
    (
      title: 'Fare quote',
      command: "curl -s -X POST $baseUrl/v1/pricing/quote $auth -H 'Content-Type: application/json' "
          "-d '{\"tier\":\"sedan\",\"pickup\":{\"lat\":12.9716,\"lng\":77.5946},\"dropoff\":{\"lat\":13.1989,\"lng\":77.7068}}'",
    ),
    (
      title: 'Book in plain English',
      command: "curl -s -X POST $baseUrl/v1/agents/booking/parse $auth -H 'Content-Type: application/json' "
          "-d '{\"rider_id\":\"rider_priya\",\"text\":\"take me to the airport at 6pm\"}'",
    ),
    (title: 'Agent swarm status', command: 'curl -s $baseUrl/v1/agents $auth'),
    (title: 'Rate-limit headers', command: 'curl -s -D - -o /dev/null $baseUrl/v1/metrics/dispatch $auth | grep -i x-ratelimit'),
    (
      title: 'Live stream (websocat)',
      command: "websocat '$ws/v1/stream?api_key=$k' <<< '{\"action\":\"subscribe\",\"topics\":[\"ops\"]}'",
    ),
  ];
}
