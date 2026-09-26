#include <jni.h>
#include <stdlib.h>
#include "_cgo_export.h"

JNIEXPORT jlong JNICALL Java_local_openflux_client_TunBridge_create(JNIEnv *env, jclass cls, jint fd, jint port, jint mtu) {
    return (jlong)OpenFluxTunCreate(fd, port, mtu);
}
JNIEXPORT jint JNICALL Java_local_openflux_client_TunBridge_run(JNIEnv *env, jclass cls, jlong handle) {
    return OpenFluxTunRun((unsigned long long)handle);
}
JNIEXPORT void JNICALL Java_local_openflux_client_TunBridge_stop(JNIEnv *env, jclass cls, jlong handle) {
    OpenFluxTunStop((unsigned long long)handle);
}
JNIEXPORT void JNICALL Java_local_openflux_client_TunBridge_destroy(JNIEnv *env, jclass cls, jlong handle) {
    OpenFluxTunDestroy((unsigned long long)handle);
}
JNIEXPORT jstring JNICALL Java_local_openflux_client_TunBridge_stats(JNIEnv *env, jclass cls, jlong handle) {
    char *text = OpenFluxTunStats((unsigned long long)handle);
    jstring result = (*env)->NewStringUTF(env, text);
    free(text);
    return result;
}
JNIEXPORT jstring JNICALL Java_local_openflux_client_TunBridge_lastError(JNIEnv *env, jclass cls) {
    char *text = OpenFluxTunError();
    jstring result = (*env)->NewStringUTF(env, text);
    free(text);
    return result;
}
