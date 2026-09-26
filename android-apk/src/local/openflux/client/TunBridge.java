package local.openflux.client;

final class TunBridge {
    static { System.loadLibrary("openfluxtun"); }
    static native long create(int fd, int socksPort, int mtu);
    static native int run(long handle);
    static native void stop(long handle);
    static native void destroy(long handle);
    static native String stats(long handle);
    static native String lastError();
    private TunBridge() {}
}
