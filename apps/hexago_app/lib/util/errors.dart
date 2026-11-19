import 'package:dio/dio.dart';

/// Whether [error] means the backend (dispatch-go or agents-py on Render's
/// free tier) is asleep or still starting: no connection, a timeout, a
/// 502/503/504, or the gateway's `{"error":"agents_waking"}`.
bool isWakeError(Object? error) {
  if (error is! DioException) return false;
  switch (error.type) {
    case DioExceptionType.connectionError:
    case DioExceptionType.connectionTimeout:
    case DioExceptionType.receiveTimeout:
    case DioExceptionType.sendTimeout:
      return true;
    case DioExceptionType.unknown:
      return error.response == null; // e.g. XMLHttpRequest onError on the web
    default:
      break;
  }
  final code = error.response?.statusCode;
  if (code == 502 || code == 503 || code == 504) return true;
  return _errorField(error.response?.data) == 'agents_waking';
}

String? _errorField(Object? data) {
  if (data is Map) {
    final e = data['error'];
    return e is String ? e : null;
  }
  return null;
}

/// A short, human message for [error]. Never the raw exception text: Dio's
/// `toString()` dumps request options and stack-ish detail into the UI.
String friendlyError(Object? error, {String action = 'reach HexaGo'}) {
  if (isWakeError(error)) return 'HexaGo is waking up. Please try again in a few seconds.';
  if (error is DioException) {
    final code = error.response?.statusCode;
    final msg = _errorField(error.response?.data);
    if (msg != null && msg.isNotEmpty && msg.length <= 200) return 'Could not $action: $msg';
    if (code != null) return 'Could not $action (HTTP $code). Please try again.';
    if (error.type == DioExceptionType.cancel) return 'Request cancelled.';
    return 'Could not $action. Please check your connection and try again.';
  }
  return 'Could not $action. Please try again.';
}
