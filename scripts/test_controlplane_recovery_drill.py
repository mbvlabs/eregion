"""Name-safety tests for the control-plane recovery drill.

These run without PostgreSQL or Docker. They pin the guard that prevents the
drill from dropping or creating a database outside its generated prefix.
"""

from __future__ import annotations

import unittest

import controlplane_recovery_drill as drill


class DrillNameTests(unittest.TestCase):
    def test_generated_names_carry_the_protective_prefix(self) -> None:
        for _ in range(20):
            name = drill.new_drill_name()
            self.assertTrue(name.startswith(drill.DRILL_PREFIX))
            drill.assert_drill_name(name)

    def test_outside_names_are_refused(self) -> None:
        for name in (
            "postgres",
            "eregion",
            "dev-postgres",
            "eregion_control_plane",
            drill.DRILL_PREFIX,
            drill.DRILL_PREFIX + "0123456789abcdef0",
            drill.DRILL_PREFIX + "0123456789ABCDEF",
            drill.DRILL_PREFIX + "0123456789abcde",
        ):
            with self.subTest(name=name):
                with self.assertRaises(drill.DrillFailure):
                    drill.assert_drill_name(name)

    def test_drop_refuses_a_name_this_drill_did_not_create(self) -> None:
        dsn = "postgres://role:password@127.0.0.1:5432/eregion?sslmode=disable"
        with self.assertRaises(drill.DrillFailure):
            drill.drop_database(dsn, drill.new_drill_name())


if __name__ == "__main__":
    unittest.main()
