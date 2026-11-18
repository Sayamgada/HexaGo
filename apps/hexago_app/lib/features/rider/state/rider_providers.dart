import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../config/app_config.dart';
import '../data/booking_api.dart';

/// Mock (offline demo) or the live gateway, like the Ops and Safety sources.
final bookingApiProvider = Provider<BookingApi>((ref) => AppConfig.useMock
    ? MockBookingApi()
    : DioBookingApi(apiBaseUrl: AppConfig.apiBaseUrl, wsBaseUrl: AppConfig.wsBaseUrl));
