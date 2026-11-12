import importlib.util
import pathlib
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location('edition_package', pathlib.Path(__file__).parents[1] / 'package.py')
package = importlib.util.module_from_spec(spec)
spec.loader.exec_module(package)


class EditionTests(unittest.TestCase):
    def test_package_rejects_binary_from_another_edition(self):
        with mock.patch.object(package, 'run', return_value='{"edition":"full"}'):
            with self.assertRaisesRegex(ValueError, 'edition'):
                package.check_edition('fixture', 'feishu')
            package.check_edition('fixture', 'full')

    def test_edition_probe_is_offline_product_command(self):
        with mock.patch.object(package, 'run', return_value='{"edition":"feishu"}') as run:
            package.check_edition('fixture', 'feishu')
            run.assert_called_once_with(['fixture', 'feishu', 'edition'])
