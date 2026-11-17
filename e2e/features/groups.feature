Feature: Groups

  Background:
    Given a domus server with the admin "demo" and the Hue bridge paired
    And I am logged in as "demo"

  Scenario: Creating a group that is exposed as a light
    When I go to the "Groups" tab
    And I name the new group "Garmin"
    And I pick the lights "Ceiling" and "Desk lamp"
    And I expose the group as a light
    And I save the group
    Then I should see the toast "Group “Garmin” saved"
    And the group list should show "Garmin" with 2 members
