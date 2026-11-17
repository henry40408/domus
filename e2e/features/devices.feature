Feature: Devices

  Background:
    Given a domus server with the admin "demo" and the Hue bridge paired
    And I am logged in as "demo"
    And I go to the "Devices" tab

  Scenario: Switching a light off
    Then the light "Ceiling" should be on
    When I switch the light "Ceiling"
    Then the light "Ceiling" should be off

  Scenario: Activating a scene
    When I activate the scene "Relax"
    Then I should see the toast "Activated Living Room: Relax"
