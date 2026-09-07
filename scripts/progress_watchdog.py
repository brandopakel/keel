"""Main-thread Unix watchdog that unwinds normal owned-process cleanup."""
import faulthandler
import math
import signal


class ProgressWatchdog:
    def __init__(self, seconds, trace):
        if not math.isfinite(seconds) or seconds <= 0:
            raise ValueError('watchdog duration must be finite and positive')
        self.seconds = seconds
        self.trace = trace
        self.phase = 'startup'

    def __enter__(self):
        if signal.getitimer(signal.ITIMER_REAL) != (0.0, 0.0):
            raise RuntimeError('progress watchdog requires an unused real-time timer')
        self.previous_handler = signal.signal(signal.SIGALRM, self.expired)
        self.beat('startup')
        return self

    def beat(self, phase):
        self.phase = phase
        signal.setitimer(signal.ITIMER_REAL, self.seconds)

    def expired(self, signum, frame):
        faulthandler.dump_traceback(file=self.trace, all_threads=True)
        raise TimeoutError(f'soak made no bounded progress for {self.seconds}s during {self.phase}')

    def __exit__(self, kind, value, tb):
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, self.previous_handler)
