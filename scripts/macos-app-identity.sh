# Sourced by the build scripts. Qualification has one isolated, persistent app
# identity; it must not compete with production or bare CLI development builds.
case ${PULSE_APP_FLAVOR:-production} in
  production) pulse_app_id=dev.pulse.provider; pulse_app_name='Pulse Provider' ;;
  qualification) pulse_app_id=dev.pulse.provider.qualification; pulse_app_name='Pulse Provider Verification' ;;
  *) echo 'PULSE_APP_FLAVOR must be production or qualification.' >&2; exit 2 ;;
esac
