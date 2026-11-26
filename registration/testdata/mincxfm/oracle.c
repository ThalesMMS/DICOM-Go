/* Build with: cc oracle.c -lminc2 -o oracle
 * Run from this folder: ./oracle linear.xfm
 * The three input landmarks and printed output are LPS millimetres.
 * libminc reads and applies the XFM in its native RAS coordinates.
 */
#include <stdio.h>
#include <volume_io.h>

int main(int argc, char **argv)
{
    VIO_General_transform transform;
    const double points[][3] = {
        {0.0, 0.0, 0.0}, {1.5, -2.25, 3.75}, {-7.0, 9.5, -11.25}
    };
    if (argc != 2 || input_transform_file(argv[1], &transform) != VIO_OK)
        return 1;
    for (unsigned i = 0; i < sizeof(points) / sizeof(points[0]); i++) {
        VIO_Real x, y, z;
        if (general_transform_point(&transform, -points[i][0], -points[i][1],
                                    points[i][2], &x, &y, &z) != VIO_OK) {
            delete_general_transform(&transform);
            return 2;
        }
        printf("%.17g %.17g %.17g\n", -x, -y, z);
    }
    delete_general_transform(&transform);
    return 0;
}
