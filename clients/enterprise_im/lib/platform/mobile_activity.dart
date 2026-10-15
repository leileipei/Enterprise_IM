import 'native_activity.dart';

// Mobile startup uses this boundary; desktop keeps its explicit view focus.
NativeActivityAdapter mobileActivityAdapter() =>
    NativeActivityAdapter(focusFromLifecycle: true);
