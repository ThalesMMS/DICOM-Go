// Synthetic protocol peer authored for dicom-go's optional interoperability
// tests. The imported transport classes are the external dcm4che oracle.
import java.io.*;
import java.net.*;
import java.nio.charset.StandardCharsets;
import org.dcm4che3.hl7.MLLPConnection;
import org.dcm4che3.hl7.MLLPInputStream;
import org.dcm4che3.hl7.MLLPOutputStream;
import org.dcm4che3.hl7.MLLPRelease;

public final class MLLPPeer {
    private static byte[] message(String id) {
        return ("MSH|^~\\&|JAVA|LAB|GO|RAD|20261009||ADT^A08|" + id
                + "|P|2.5.1\rEVN|A08\rPID|1||SYNTHETIC-MRN\r").getBytes(StandardCharsets.US_ASCII);
    }
    private static byte[] applicationACK(byte[] request, String code) {
        String id = new String(request, StandardCharsets.US_ASCII).split("\r", 2)[0].split("\\|", -1)[9];
        return ("MSH|^~\\&|JAVA|LAB|GO|RAD|20261009||ACK^A08^ACK|JAVA-ACK|P|2.5.1\rMSA|"
                + code + "|" + id + "\r").getBytes(StandardCharsets.US_ASCII);
    }
    private static void check(boolean value) throws IOException {
        if (!value) throw new IOException("synthetic peer protocol assertion failed");
    }
    public static void main(String[] args) throws Exception {
        String role = args[0], flavor = args[2];
        MLLPRelease release = args[1].equals("1") ? MLLPRelease.MLLP1 : MLLPRelease.MLLP2;
        if (role.equals("server")) {
            try (ServerSocket listener = new ServerSocket(0, 1, InetAddress.getByName("127.0.0.1"))) {
                listener.setSoTimeout(4000);
                System.out.println(listener.getLocalPort()); System.out.flush();
                try (Socket socket = listener.accept()) {
                    socket.setSoTimeout(4000);
                    if (flavor.equals("AA") || flavor.equals("AE") || flavor.equals("AR")) {
                        try (MLLPConnection connection = new MLLPConnection(socket, release)) {
                            byte[] request = connection.readMessage();
                            check(request != null);
                            connection.writeMessage(applicationACK(request, flavor));
                        }
                    } else {
                        MLLPInputStream in = new MLLPInputStream(socket.getInputStream());
                        MLLPOutputStream out = new MLLPOutputStream(socket.getOutputStream());
                        check(in.readMessage() != null);
                        switch (flavor) {
                        case "nak": out.writeMessage(new byte[]{0x15}); break;
                        case "malformed": out.writeMessage(new byte[]{'X'}); break;
                        case "rawack": socket.getOutputStream().write(0x06); break;
                        case "truncated": socket.getOutputStream().write(new byte[]{0x0b, 0x06, 0x1c}); break;
                        case "ack-close": out.writeMessage(new byte[]{0x06}); break;
                        case "ack-timeout": out.writeMessage(new byte[]{0x06});
                        case "timeout": while (socket.getInputStream().read() != -1) {} break;
                        case "close": break;
                        default: throw new IOException("unknown fixture mode");
                        }
                    }
                }
            }
        } else {
            try (Socket socket = new Socket("127.0.0.1", Integer.parseInt(args[3]))) {
                socket.setSoTimeout(4000);
                if (flavor.equals("nak-response")) {
                    MLLPInputStream in = new MLLPInputStream(socket.getInputStream());
                    MLLPOutputStream out = new MLLPOutputStream(socket.getOutputStream());
                    out.writeMessage(message("JAVA-NAK"));
                    byte[] confirm = in.readMessage();
                    check(confirm != null && confirm.length == 1 && confirm[0] == 0x06);
                    byte[] app = in.readMessage();
                    check(app != null && new String(app, StandardCharsets.US_ASCII).contains("MSA|AA|JAVA-NAK"));
                    out.writeMessage(new byte[]{0x15});
                    check(in.readMessage() == null);
                } else {
                    try (MLLPConnection connection = new MLLPConnection(socket, release)) {
                        for (int i = 1; i <= 2; i++) {
                            String id = "JAVA" + i;
                            connection.writeMessage(message(id));
                            byte[] app = connection.readMessage();
                            check(app != null && new String(app, StandardCharsets.US_ASCII).contains("MSA|AA|" + id));
                        }
                    }
                }
            }
        }
        System.out.println("ok");
    }
}
